// Package fw 是面板的「防火墙」:认出这台机器上管入站的是哪种系统防火墙,把面板里的规则落成内核规则,
// 以及安装 / 面板上那一步「放开系统防火墙」。
//
// 三件事分得很开,互不替代:
//
//  1. 系统自己的防火墙(ufw / firewalld / 发行版带的 iptables 规则 / 原生 nftables)默认会把没放行的端口挡在外面 ——
//     云控制台放行了、面板和线路还是连不上,多半是它。面板只做两种动作:「放开」(见 open.go,可逆、先备份)和
//     「放行 m-ui 用到的端口」(往它自己的规则里加放行,只加不删别人的)。不清空、不改默认策略、不替用户决定开不开。
//  2. 面板的规则(拉黑来源、某些端口只允许某些来源)落在 m-ui 自己的一张表里:nft 的 inet m_ui(input 钩子,优先级 -10),
//     没有 nft 的机器用 iptables 的自建链 M-UI-IN(挂在 INPUT 第一条)。丢包在哪张表里都是终局,所以不管那台用的是
//     ufw、firewalld 还是 iptables,这里的拒绝都生效;每次变动整表重建(一次事务),不留半截状态。
//  3. 应用层的黑名单(面板登录、订阅、外部 API 的来源)不在这个包里:那是各监听自己断开连接,不动代理流量。
//
// 命令一律按参数数组执行,不过 shell;规则里的端口、网段先解析成结构再写出去,不拼用户给的字符串。
package fw

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Runner 执行系统命令。测试里换成假的,真机上是 Exec。
type Runner interface {
	// Run 执行 name args...,stdin 不空时喂给它;返回标准输出(出错时 err 里带标准错误的末尾)。
	Run(stdin, name string, args ...string) (string, error)
	// Has 这个命令在不在 PATH 里。
	Has(name string) bool
}

// Exec 真的去执行(每条命令最多等 20 秒:防火墙命令卡住不能把面板拖死)。
type Exec struct{}

func (Exec) Has(name string) bool { _, err := exec.LookPath(name); return err == nil }

func (Exec) Run(stdin, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 400 {
			msg = msg[len(msg)-400:]
		}
		if msg == "" {
			return out.String(), fmt.Errorf("%s: %w", name, err)
		}
		return out.String(), fmt.Errorf("%s: %s", name, msg)
	}
	return out.String(), nil
}

// 系统防火墙的种类。
const (
	FrontNone      = "none"      // 没有会拦入站的东西
	FrontUFW       = "ufw"       // ufw 启用中
	FrontFirewalld = "firewalld" // firewalld 运行中
	FrontIPTables  = "iptables"  // 没有前端,但 iptables 的 INPUT 里有会拦的(策略 DROP,或有 DROP / REJECT 规则:Oracle 的镜像就是这样)
	FrontNFTables  = "nftables"  // 原生 nftables 的 input 钩子里有会拦的(不是 m-ui 自己那张表)
)

// m-ui 自己的规则落在哪。
const (
	EngineNFT      = "nft"
	EngineIPTables = "iptables"
	EngineNone     = "" // 两样都没有:面板规则做不了(应用层黑名单不受影响)
)

// Status 这台机器的防火墙状况。
type Status struct {
	Frontend string `json:"frontend"`
	Blocking bool   `json:"blocking"` // 现在会不会把没放行的入站挡掉
	Detail   string `json:"detail"`   // 给人看的一句依据(INPUT 策略 DROP、规则里有 REJECT……)
	Opened   bool   `json:"opened"`   // INPUT 最前面有 m-ui 加的那条全放行(「放开系统防火墙」做的)
	Engine   string `json:"engine"`
}

const (
	openComment  = "m-ui-open"  // 「放开」加的那条全放行
	allowComment = "m-ui-allow" // 「放行端口」加的规则
	tableName    = "m_ui"
	chainName    = "M-UI-IN"
)

// Detect 认出系统防火墙。前端先于底层:ufw、firewalld 自己也是往 iptables / nft 里写规则的。
func Detect(r Runner) Status {
	st := Status{Frontend: FrontNone, Engine: engine(r)}
	if r.Has("ufw") {
		if out, err := r.Run("", "ufw", "status"); err == nil && strings.Contains(strings.ToLower(firstLine(out)), "active") && !strings.Contains(strings.ToLower(firstLine(out)), "inactive") {
			st.Frontend, st.Blocking, st.Detail = FrontUFW, true, "ufw active"
			return st
		}
	}
	if r.Has("firewall-cmd") {
		if out, err := r.Run("", "firewall-cmd", "--state"); err == nil && strings.TrimSpace(out) == "running" {
			st.Frontend, st.Blocking, st.Detail = FrontFirewalld, true, "firewalld running"
			return st
		}
	}
	for _, bin := range []string{"iptables", "ip6tables"} {
		if !r.Has(bin) {
			continue
		}
		out, err := r.Run("", bin, "-S", "INPUT")
		if err != nil {
			continue
		}
		opened, why := inputBlocks(out)
		if opened {
			st.Opened = true
		}
		if why != "" && st.Frontend == FrontNone {
			st.Frontend, st.Detail = FrontIPTables, bin+": "+why
			st.Blocking = !opened
		}
	}
	if st.Frontend != FrontNone {
		return st
	}
	if r.Has("nft") {
		if out, err := r.Run("", "nft", "list", "ruleset"); err == nil {
			if why := nftBlocks(out); why != "" {
				st.Frontend, st.Blocking, st.Detail = FrontNFTables, true, why
			}
		}
	}
	return st
}

func engine(r Runner) string {
	if r.Has("nft") {
		if _, err := r.Run("", "nft", "list", "tables"); err == nil {
			return EngineNFT
		}
	}
	if r.Has("iptables") {
		return EngineIPTables
	}
	return EngineNone
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// inputBlocks 读 `iptables -S INPUT`:最前面是不是 m-ui 那条全放行;INPUT 会不会拦(策略 DROP,或有 DROP / REJECT 规则)。
// m-ui 自己的东西(跳到 M-UI-IN、带 m-ui 注释的)不算「系统防火墙在拦」。
func inputBlocks(out string) (opened bool, why string) {
	first := true
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "-P INPUT ") {
			if strings.HasSuffix(line, " DROP") || strings.HasSuffix(line, " REJECT") {
				why = "INPUT policy " + strings.TrimPrefix(line, "-P INPUT ")
			}
			continue
		}
		if !strings.HasPrefix(line, "-A INPUT") {
			continue
		}
		mine := strings.Contains(line, "m-ui") || strings.Contains(line, "-j "+chainName)
		if first && strings.Contains(line, openComment) && strings.HasSuffix(line, "-j ACCEPT") {
			opened = true
		}
		if !mine {
			first = false
			if why == "" && (strings.Contains(line, "-j DROP") || strings.Contains(line, "-j REJECT")) {
				why = "INPUT has a DROP / REJECT rule"
			}
		}
	}
	return opened, why
}

// nftBlocks 读 `nft list ruleset`:除了 m-ui 自己那张表,有没有挂在 input 钩子上、策略是 drop 的基链。
// 只认策略:规则里零星的 drop(限速、反扫描)不等于「没放行的端口进不来」。
func nftBlocks(out string) string {
	table := ""
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == "table" {
			table = f[1] + " " + f[2]
			continue
		}
		if table == "inet "+tableName || table == "" {
			continue
		}
		// 由 iptables-nft 管着的表(ip filter / ip6 filter)已经在 iptables 那一步看过了
		if (table == "ip filter" || table == "ip6 filter") && strings.Contains(out, "# Warning: table "+table) {
			continue
		}
		if strings.Contains(line, "hook input") && strings.Contains(line, "policy drop") {
			return "nftables: table " + table + " drops input by default"
		}
	}
	return ""
}

// isSep 列表里能当分隔的字符:逗号(半角、全角)、顿号、分号、空白。
func isSep(r rune) bool {
	switch r {
	case ',', '，', '、', ';', ' ', '\n', '\t', '\r':
		return true
	}
	return false
}

// PortRange 一个端口或一段端口(闭区间)。
type PortRange struct{ From, To int }

func (p PortRange) String() string {
	if p.From == p.To {
		return strconv.Itoa(p.From)
	}
	return strconv.Itoa(p.From) + "-" + strconv.Itoa(p.To)
}

// ParsePorts 解析「443, 8000-9000」这样的端口列表(逗号、空格、换行都能分隔,区间用 - 或 :);空串 = 全部端口,返回 nil。
func ParsePorts(s string) ([]PortRange, error) {
	var out []PortRange
	for _, part := range strings.FieldsFunc(s, isSep) {
		a, b, isRange := strings.Cut(strings.ReplaceAll(part, ":", "-"), "-")
		lo, err1 := strconv.Atoi(a)
		hi, err2 := lo, error(nil)
		if isRange {
			hi, err2 = strconv.Atoi(b)
		}
		if err1 != nil || err2 != nil || lo < 1 || hi > 65535 || lo > hi {
			return nil, fmt.Errorf("「%s」不是端口或端口区间(例:443、8000-9000)", part)
		}
		out = append(out, PortRange{lo, hi})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].From < out[j].From || (out[i].From == out[j].From && out[i].To < out[j].To)
	})
	return out, nil
}

// ParseSources 解析来源列表:IP 或网段,逗号 / 空格 / 换行分隔。单个 IP 当成 /32 或 /128;网段按掩码取整。
func ParseSources(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	seen := map[netip.Prefix]bool{}
	for _, part := range strings.FieldsFunc(s, isSep) {
		var p netip.Prefix
		if a, err := netip.ParseAddr(part); err == nil && a.Zone() == "" {
			a = a.Unmap()
			p = netip.PrefixFrom(a, a.BitLen())
		} else if pp, err := netip.ParsePrefix(part); err == nil {
			p = netip.PrefixFrom(pp.Addr().Unmap(), pp.Bits()).Masked()
		} else {
			return nil, fmt.Errorf("「%s」不是 IP 或网段(例:1.2.3.4、203.0.113.0/24、2001:db8::/32)", part)
		}
		if p.Bits() == 0 {
			return nil, fmt.Errorf("「%s」是全部地址,不能当来源写进规则", part)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

// 规则的两种做法。
const (
	KindDeny = "deny" // 这些来源进不来(不填端口 = 整台机器都不让进)
	KindOnly = "only" // 这些端口只让这些来源进,别人一律拒绝
)

// Rule 一条已经解析好的面板规则。
type Rule struct {
	ID      uint
	Kind    string
	Proto   string // "" = TCP 和 UDP,或 tcp / udp
	Ports   []PortRange
	Sources []netip.Prefix
}

// Check 规则本身说不说得通。
func (r Rule) Check() error {
	switch r.Kind {
	case KindDeny:
		if len(r.Sources) == 0 {
			return fmt.Errorf("拉黑规则要填来源 IP 或网段")
		}
	case KindOnly:
		if len(r.Ports) == 0 {
			return fmt.Errorf("「只允许这些来源」的规则要填端口")
		}
		if len(r.Sources) == 0 {
			return fmt.Errorf("「只允许这些来源」的规则要填允许的来源")
		}
	default:
		return fmt.Errorf("不认识的规则种类 %q", r.Kind)
	}
	if r.Proto != "" && r.Proto != "tcp" && r.Proto != "udp" {
		return fmt.Errorf("协议只能是 tcp、udp 或留空(两种都算)")
	}
	return nil
}

func split46(src []netip.Prefix) (v4, v6 []string) {
	for _, p := range src {
		s := p.String()
		if p.IsSingleIP() {
			s = p.Addr().String()
		}
		if p.Addr().Is4() {
			v4 = append(v4, s)
		} else {
			v6 = append(v6, s)
		}
	}
	return v4, v6
}

func portSet(ports []PortRange) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = p.String()
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

func l4(proto string) string {
	if proto == "tcp" || proto == "udp" {
		return proto + " dport "
	}
	return "meta l4proto { tcp, udp } th dport "
}

// NFTScript 把规则写成一份 nft 脚本:整张表先删后建,`nft -f` 一次事务换上去。
// 顺序固定:回环永远放行 → 整台拉黑的来源 → 带端口的拉黑 →「只允许」的端口(先放行名单里的,再拒绝其余)。
// 所以同一个来源既在黑名单里、又在某个端口的允许名单里时,以拉黑为准。
func NFTScript(rules []Rule) string {
	var ban4, ban6, body []string
	sorted := append([]Rule(nil), rules...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, r := range sorted {
		if r.Kind != KindDeny || len(r.Ports) > 0 || r.Proto != "" {
			continue
		}
		a, b := split46(r.Sources)
		ban4, ban6 = append(ban4, a...), append(ban6, b...)
	}
	for _, r := range sorted {
		if r.Kind != KindDeny || (len(r.Ports) == 0 && r.Proto == "") {
			continue
		}
		v4, v6 := split46(r.Sources)
		match := ""
		if len(r.Ports) > 0 {
			match = l4(r.Proto) + portSet(r.Ports) + " "
		} else {
			match = "meta l4proto " + r.Proto + " "
		}
		if len(v4) > 0 {
			body = append(body, fmt.Sprintf("ip saddr { %s } %sdrop comment \"rule %d\"", strings.Join(v4, ", "), match, r.ID))
		}
		if len(v6) > 0 {
			body = append(body, fmt.Sprintf("ip6 saddr { %s } %sdrop comment \"rule %d\"", strings.Join(v6, ", "), match, r.ID))
		}
	}
	for _, r := range sorted {
		if r.Kind != KindOnly {
			continue
		}
		v4, v6 := split46(r.Sources)
		match := l4(r.Proto) + portSet(r.Ports)
		if len(v4) > 0 {
			body = append(body, fmt.Sprintf("%s ip saddr { %s } accept comment \"rule %d\"", match, strings.Join(v4, ", "), r.ID))
		}
		if len(v6) > 0 {
			body = append(body, fmt.Sprintf("%s ip6 saddr { %s } accept comment \"rule %d\"", match, strings.Join(v6, ", "), r.ID))
		}
		body = append(body, fmt.Sprintf("%s drop comment \"rule %d\"", match, r.ID))
	}
	var b strings.Builder
	// 先声明再删:表不存在时直接 delete 会报错,整个事务就废了
	fmt.Fprintf(&b, "table inet %s\ndelete table inet %s\n", tableName, tableName)
	if len(ban4)+len(ban6)+len(body) == 0 {
		return b.String() // 一条规则都没有:表也不留
	}
	fmt.Fprintf(&b, "table inet %s {\n", tableName)
	if len(ban4) > 0 {
		fmt.Fprintf(&b, "\tset ban4 {\n\t\ttype ipv4_addr\n\t\tflags interval\n\t\tauto-merge\n\t\telements = { %s }\n\t}\n", strings.Join(ban4, ", "))
	}
	if len(ban6) > 0 {
		fmt.Fprintf(&b, "\tset ban6 {\n\t\ttype ipv6_addr\n\t\tflags interval\n\t\tauto-merge\n\t\telements = { %s }\n\t}\n", strings.Join(ban6, ", "))
	}
	b.WriteString("\tchain input {\n\t\ttype filter hook input priority -10; policy accept;\n\t\tiifname \"lo\" accept\n")
	if len(ban4) > 0 {
		b.WriteString("\t\tip saddr @ban4 drop\n")
	}
	if len(ban6) > 0 {
		b.WriteString("\t\tip6 saddr @ban6 drop\n")
	}
	for _, line := range body {
		b.WriteString("\t\t" + line + "\n")
	}
	b.WriteString("\t}\n}\n")
	return b.String()
}

// multiport iptables 的 multiport 一条最多 15 个端口(一段算两个):超了拆成几条。
func multiport(ports []PortRange) [][]string {
	var out [][]string
	var cur []string
	n := 0
	for _, p := range ports {
		cost, s := 1, strconv.Itoa(p.From)
		if p.From != p.To {
			cost, s = 2, strconv.Itoa(p.From)+":"+strconv.Itoa(p.To)
		}
		if n+cost > 15 {
			out, cur, n = append(out, cur), nil, 0
		}
		cur, n = append(cur, s), n+cost
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// IPTablesScript 没有 nft 的机器:给 iptables-restore --noflush 用的脚本(v6 = true 出 ip6tables 那份)。
// 只动自己的链 M-UI-IN:先清空再按顺序写,COMMIT 时一次换上。顺序同 NFTScript。
func IPTablesScript(rules []Rule, v6 bool) string {
	pick := func(src []netip.Prefix) []string {
		a, b := split46(src)
		if v6 {
			return b
		}
		return a
	}
	protos := func(p string) []string {
		if p == "" {
			return []string{"tcp", "udp"}
		}
		return []string{p}
	}
	sorted := append([]Rule(nil), rules...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	lines := []string{"-A " + chainName + " -i lo -j RETURN"}
	for _, r := range sorted {
		if r.Kind != KindDeny {
			continue
		}
		for _, s := range pick(r.Sources) {
			switch {
			case len(r.Ports) == 0 && r.Proto == "":
				lines = append(lines, fmt.Sprintf("-A %s -s %s -j DROP", chainName, s))
			case len(r.Ports) == 0:
				lines = append(lines, fmt.Sprintf("-A %s -s %s -p %s -j DROP", chainName, s, r.Proto))
			default:
				for _, pr := range protos(r.Proto) {
					for _, mp := range multiport(r.Ports) {
						lines = append(lines, fmt.Sprintf("-A %s -s %s -p %s -m multiport --dports %s -j DROP", chainName, s, pr, strings.Join(mp, ",")))
					}
				}
			}
		}
	}
	for _, r := range sorted {
		if r.Kind != KindOnly {
			continue
		}
		for _, pr := range protos(r.Proto) {
			for _, mp := range multiport(r.Ports) {
				ports := strings.Join(mp, ",")
				for _, s := range pick(r.Sources) {
					lines = append(lines, fmt.Sprintf("-A %s -s %s -p %s -m multiport --dports %s -j RETURN", chainName, s, pr, ports))
				}
				lines = append(lines, fmt.Sprintf("-A %s -p %s -m multiport --dports %s -j DROP", chainName, pr, ports))
			}
		}
	}
	return "*filter\n:" + chainName + " - [0:0]\n-F " + chainName + "\n" + strings.Join(lines, "\n") + "\nCOMMIT\n"
}

// Apply 把规则落到内核。nft 整表替换;没有 nft 用 iptables 自建链。engine 为空(两样都没有)时报错。
// 没有任何规则时把自己的表 / 链撤掉,不留空壳。
func Apply(r Runner, rules []Rule) error {
	for _, x := range rules {
		if err := x.Check(); err != nil {
			return fmt.Errorf("规则 #%d:%w", x.ID, err)
		}
	}
	switch engine(r) {
	case EngineNFT:
		_, err := r.Run(NFTScript(rules), "nft", "-f", "-")
		return err
	case EngineIPTables:
		return applyIPTables(r, rules)
	}
	return fmt.Errorf("这台机器上没有 nft 也没有 iptables,面板规则落不下去")
}

func applyIPTables(r Runner, rules []Rule) error {
	for _, v6 := range []bool{false, true} {
		bin := "iptables"
		if v6 {
			bin = "ip6tables"
		}
		if !r.Has(bin) || !r.Has(bin+"-restore") {
			if v6 {
				continue // 没装 ip6tables 的机器只管 v4
			}
			return fmt.Errorf("缺少 %s / %s-restore", bin, bin)
		}
		if len(rules) == 0 { // 撤掉:摘钩子、清空、删链(本来就没有的不算错)
			_, _ = r.Run("", bin, "-D", "INPUT", "-j", chainName)
			_, _ = r.Run("", bin, "-F", chainName)
			_, _ = r.Run("", bin, "-X", chainName)
			continue
		}
		if _, err := r.Run(IPTablesScript(rules, v6), bin+"-restore", "--noflush"); err != nil {
			return err
		}
		if _, err := r.Run("", bin, "-C", "INPUT", "-j", chainName); err != nil {
			if _, err := r.Run("", bin, "-I", "INPUT", "1", "-j", chainName); err != nil {
				return err
			}
		}
	}
	return nil
}

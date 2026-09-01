// Package importer 从旧 s-ui 数据库一次性迁移到 m-ui 数据模型。
//
// 迁移映射:
//   - 入站(hysteria2/anytls/shadowsocks)+ 路由规则 → 线路(Line),id/端口/名称原样保留
//   - 出站(direct 除外)→ 上游(Upstream),id 原样保留
//   - 客户端 → 用户(User)+ 用户线路关系(UserLine),凭据/配额/流量/重置周期原样保留
//   - 面板管理员 → Admin(bcrypt 哈希原样)
//   - webDomain → 本机入口服务器(Node)
//
// 端口、凭据、名称全部原样,使既有客户端配置在割接后无需刷新订阅即可继续连接。
package importer

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/fangjunsheng555/m-ui/database"
	"github.com/fangjunsheng555/m-ui/database/model"

	"gorm.io/gorm"
)

// 支持迁移的入站协议;其余类型报告并跳过。
var supportedLineProtocols = map[string]bool{
	"hysteria2":   true,
	"anytls":      true,
	"shadowsocks": true,
}

// 用户凭据里保留的协议 key(shadowsocks16 是 2022-blake3-aes-128-gcm 的客户端配置键)。
var keptCredentialKeys = []string{"hysteria2", "anytls", "shadowsocks", "shadowsocks16"}

// 从旧 settings 原样复制的键。
var copiedSettingKeys = []string{
	"webListen", "webPort", "webPath", "webDomain", "webCertFile", "webKeyFile",
	"sessionMaxAge", "trafficAge", "timeLocation", "statsBucketSeconds",
	"subPath", "subEncode", "subShowInfo", "subUpdates",
}

type Report struct {
	Lines     int
	Upstreams int
	Users     int
	UserLines int
	Warnings  []string
	Sections  []string
}

func (r *Report) warnf(format string, a ...interface{}) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, a...))
}

func (r *Report) section(format string, a ...interface{}) {
	r.Sections = append(r.Sections, fmt.Sprintf(format, a...))
}

func Run(from, to, orderFile, profileTitle string, force bool) error {
	if _, err := os.Stat(to); err == nil {
		if !force {
			return fmt.Errorf("目标 %s 已存在,如需覆盖请加 -force", to)
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			os.Remove(to + suffix)
		}
	}

	src, err := database.OpenReadOnly(from)
	if err != nil {
		return fmt.Errorf("打开源库: %w", err)
	}
	dst, err := database.Open(to)
	if err != nil {
		return fmt.Errorf("创建目标库: %w", err)
	}

	report := &Report{}
	orderRank, err := loadOrder(orderFile)
	if err != nil {
		return err
	}

	err = dst.Transaction(func(tx *gorm.DB) error {
		upstreamIdByTag, err := importUpstreams(src, tx, report)
		if err != nil {
			return err
		}
		lineIds, err := importLines(src, tx, report, upstreamIdByTag, orderRank)
		if err != nil {
			return err
		}
		if err := importUsers(src, tx, report, lineIds); err != nil {
			return err
		}
		if err := importAdmin(src, tx, report); err != nil {
			return err
		}
		if err := importSettings(src, tx, report, profileTitle); err != nil {
			return err
		}
		return importLocalNode(src, tx, report)
	})
	if err != nil {
		return err
	}

	if err := verify(src, dst, report); err != nil {
		return err
	}
	// 检查点并关闭:确保数据全部落进 .db 文件本身,
	// 使"复制单个文件 = 完整备份"这一前提成立。
	if err := database.Close(dst); err != nil {
		return fmt.Errorf("收尾写入数据库: %w", err)
	}
	return writeReport(to, report)
}

// ---- 上游 ----

type oldOutbound struct {
	Id      uint
	Type    string
	Tag     string
	Options []byte
}

func importUpstreams(src, dst *gorm.DB, report *Report) (map[string]uint, error) {
	var outbounds []oldOutbound
	if err := src.Raw("SELECT id,type,tag,options FROM outbounds ORDER BY id").Scan(&outbounds).Error; err != nil {
		return nil, fmt.Errorf("读取出站: %w", err)
	}
	idByTag := make(map[string]uint, len(outbounds))
	sortIdx := 0
	for _, ob := range outbounds {
		if ob.Type == "direct" {
			idByTag[ob.Tag] = 0 // 内置 direct
			continue
		}
		sortIdx++
		up := model.Upstream{
			Id:      ob.Id,
			Name:    ob.Tag,
			Type:    ob.Type,
			Options: json.RawMessage(ob.Options),
			Sort:    sortIdx,
		}
		if err := dst.Create(&up).Error; err != nil {
			return nil, fmt.Errorf("写入上游 %q: %w", ob.Tag, err)
		}
		idByTag[ob.Tag] = ob.Id
		report.Upstreams++
	}
	return idByTag, nil
}

// ---- 线路 ----

type oldInbound struct {
	Id      uint
	Type    string
	Tag     string
	Options []byte
	Addrs   []byte
}

func importLines(src, dst *gorm.DB, report *Report, upstreamIdByTag map[string]uint, orderRank map[string]int) (map[uint]bool, error) {
	var inbounds []oldInbound
	if err := src.Raw("SELECT id,type,tag,options,addrs FROM inbounds ORDER BY id").Scan(&inbounds).Error; err != nil {
		return nil, fmt.Errorf("读取入站: %w", err)
	}
	routeMap, err := readRouteMap(src, report)
	if err != nil {
		return nil, err
	}

	lineIds := make(map[uint]bool, len(inbounds))
	type pending struct {
		line model.Line
		rank int
	}
	var pendings []pending

	for _, inb := range inbounds {
		if !supportedLineProtocols[inb.Type] {
			report.warnf("入站 #%d %q 类型 %s 不在迁移范围,已跳过", inb.Id, inb.Tag, inb.Type)
			continue
		}
		var options map[string]json.RawMessage
		if err := json.Unmarshal(inb.Options, &options); err != nil {
			return nil, fmt.Errorf("解析入站 %q options: %w", inb.Tag, err)
		}
		port := 0
		if raw, ok := options["listen_port"]; ok {
			if err := json.Unmarshal(raw, &port); err != nil || port == 0 {
				report.warnf("入站 %q 无有效 listen_port,已跳过", inb.Tag)
				continue
			}
		}
		delete(options, "listen")
		delete(options, "listen_port")
		remaining, err := json.Marshal(options)
		if err != nil {
			return nil, err
		}

		upstreamId := uint(0)
		if outTag, ok := routeMap[inb.Tag]; ok {
			if id, ok := upstreamIdByTag[outTag]; ok {
				upstreamId = id
			} else {
				report.warnf("线路 %q 的路由指向不存在的出站 %q,改为 direct", inb.Tag, outTag)
			}
		} else {
			report.warnf("线路 %q 没有路由规则,落地为 direct", inb.Tag)
		}

		rank, listed := orderRank[inb.Tag]
		if !listed {
			rank = len(orderRank) + int(inb.Id) // 未在排序文件中的排在已列出的后面,按旧 id 保持相对顺序
		}
		addrs := normalizeAddrs(inb.Addrs)
		pendings = append(pendings, pending{
			line: model.Line{
				Id:         inb.Id,
				Name:       inb.Tag,
				Protocol:   inb.Type,
				Port:       port,
				UpstreamId: upstreamId,
				Options:    json.RawMessage(remaining),
				Addrs:      addrs,
				Enabled:    true,
			},
			rank: rank,
		})
	}

	sort.SliceStable(pendings, func(i, j int) bool { return pendings[i].rank < pendings[j].rank })
	for i := range pendings {
		pendings[i].line.Sort = i + 1
		if err := dst.Create(&pendings[i].line).Error; err != nil {
			return nil, fmt.Errorf("写入线路 %q: %w", pendings[i].line.Name, err)
		}
		lineIds[pendings[i].line.Id] = true
		report.Lines++
	}

	var listing strings.Builder
	listing.WriteString("| 排序 | 线路 | 协议 | 端口 | 上游 |\n|---|---|---|---|---|\n")
	upstreamName := func(id uint) string {
		if id == 0 {
			return "direct"
		}
		var name string
		dst.Model(&model.Upstream{}).Select("name").Where("id = ?", id).Scan(&name)
		return name
	}
	for _, p := range pendings {
		fmt.Fprintf(&listing, "| %d | %s | %s | %d | %s |\n",
			p.line.Sort, p.line.Name, p.line.Protocol, p.line.Port, upstreamName(p.line.UpstreamId))
	}
	report.section("## 线路映射\n\n%s", listing.String())
	return lineIds, nil
}

// readRouteMap 从旧基础 config 的路由规则里提取 入站tag→出站tag。
func readRouteMap(src *gorm.DB, report *Report) (map[string]string, error) {
	var configStr string
	if err := src.Raw("SELECT value FROM settings WHERE key = 'config'").Scan(&configStr).Error; err != nil {
		return nil, fmt.Errorf("读取旧 config: %w", err)
	}
	var root struct {
		Route struct {
			Rules []struct {
				Action   string          `json:"action"`
				Inbound  json.RawMessage `json:"inbound"`
				Outbound string          `json:"outbound"`
			} `json:"rules"`
		} `json:"route"`
	}
	if err := json.Unmarshal([]byte(configStr), &root); err != nil {
		return nil, fmt.Errorf("解析旧 config: %w", err)
	}
	routeMap := make(map[string]string)
	for _, rule := range root.Route.Rules {
		if rule.Action != "route" || rule.Outbound == "" || len(rule.Inbound) == 0 {
			continue
		}
		for _, tag := range decodeStringList(rule.Inbound) {
			if existing, dup := routeMap[tag]; dup && existing != rule.Outbound {
				report.warnf("入站 %q 出现在多条路由规则(%q 与 %q),采用先出现的", tag, existing, rule.Outbound)
				continue
			}
			routeMap[tag] = rule.Outbound
		}
	}
	return routeMap, nil
}

// normalizeAddrs 把旧 inbound 的 addrs 列规整:空/空数组/null → nil(常态,用入口主机)。
func normalizeAddrs(raw []byte) json.RawMessage {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || s == "[]" {
		return nil
	}
	var list []map[string]interface{}
	if err := json.Unmarshal(raw, &list); err != nil || len(list) == 0 {
		return nil
	}
	return json.RawMessage(raw)
}

func decodeStringList(raw json.RawMessage) []string {
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil && single != "" {
		return []string{single}
	}
	return nil
}

// ---- 用户 ----

type oldClient struct {
	Id        uint
	Enable    bool
	Name      string
	Config    []byte
	Inbounds  []byte
	Volume    int64
	Expiry    int64
	Up        int64
	Down      int64
	TotalUp   int64
	TotalDown int64
	Desc      string
	Remark    string
	CreatedAt int64
	OnlineAt  int64
	AutoReset bool
	ResetDays int
	NextReset int64
}

func importUsers(src, dst *gorm.DB, report *Report, lineIds map[uint]bool) error {
	var clients []oldClient
	err := src.Raw(`SELECT id,enable,name,config,inbounds,volume,expiry,up,down,
		total_up,total_down,"desc",remark,created_at,online_at,
		auto_reset,reset_days,next_reset FROM clients ORDER BY id`).Scan(&clients).Error
	if err != nil {
		return fmt.Errorf("读取客户端: %w", err)
	}

	for _, c := range clients {
		credentials, err := filterCredentials(c.Config)
		if err != nil {
			report.warnf("用户 %q 凭据解析失败,原样保留: %v", c.Name, err)
			credentials = json.RawMessage(c.Config)
		}
		user := model.User{
			Id: c.Id, Enabled: c.Enable, Name: c.Name,
			Credentials: credentials,
			Volume:      c.Volume, Expiry: c.Expiry,
			Up: c.Up, Down: c.Down, TotalUp: c.TotalUp, TotalDown: c.TotalDown,
			AutoReset: c.AutoReset, ResetDays: c.ResetDays, NextReset: c.NextReset,
			Remark: c.Remark, Desc: c.Desc,
			CreatedAt: c.CreatedAt, OnlineAt: c.OnlineAt,
		}
		if err := dst.Create(&user).Error; err != nil {
			return fmt.Errorf("写入用户 %q: %w", c.Name, err)
		}
		report.Users++

		var inboundIds []uint
		if err := json.Unmarshal(c.Inbounds, &inboundIds); err != nil {
			report.warnf("用户 %q 的线路列表解析失败: %v", c.Name, err)
			continue
		}
		for _, id := range inboundIds {
			if !lineIds[id] {
				report.warnf("用户 %q 引用了未迁移的入站 #%d,已忽略", c.Name, id)
				continue
			}
			if err := dst.Create(&model.UserLine{UserId: c.Id, LineId: id}).Error; err != nil {
				return fmt.Errorf("写入用户线路 %q→#%d: %w", c.Name, id, err)
			}
			report.UserLines++
		}
	}
	return nil
}

func filterCredentials(config []byte) (json.RawMessage, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(config, &all); err != nil {
		return nil, err
	}
	kept := make(map[string]json.RawMessage)
	for _, key := range keptCredentialKeys {
		if v, ok := all[key]; ok {
			kept[key] = v
		}
	}
	return json.Marshal(kept)
}

// ---- 管理员 / 设置 / 本机节点 ----

func importAdmin(src, dst *gorm.DB, report *Report) error {
	var admins []model.Admin
	if err := src.Raw("SELECT id,username,password FROM users ORDER BY id").Scan(&admins).Error; err != nil {
		return fmt.Errorf("读取管理员: %w", err)
	}
	for i := range admins {
		if err := dst.Create(&admins[i]).Error; err != nil {
			return err
		}
	}
	report.section("## 管理员\n\n迁入 %d 个面板账号(密码哈希原样,登录口令不变)。", len(admins))
	return nil
}

func importSettings(src, dst *gorm.DB, report *Report, profileTitle string) error {
	oldSettings := map[string]string{}
	rows := []model.Setting{}
	if err := src.Raw("SELECT key,value FROM settings").Scan(&rows).Error; err != nil {
		return fmt.Errorf("读取设置: %w", err)
	}
	for _, row := range rows {
		oldSettings[row.Key] = row.Value
	}

	values := map[string]string{}
	for _, key := range copiedSettingKeys {
		if v, ok := oldSettings[key]; ok {
			values[key] = v
		}
	}
	// 订阅直出 HTTPS:对外口 2056(替代 nginx),证书沿用面板证书文件。
	values["subListen"] = "0.0.0.0"
	values["subPort"] = "2056"
	values["subCertFile"] = oldSettings["webCertFile"]
	values["subKeyFile"] = oldSettings["webKeyFile"]
	values["nodeMode"] = "false" // 主(HK)
	if profileTitle != "" {
		values["subProfileTitle"] = profileTitle
	}

	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var listing strings.Builder
	for _, k := range keys {
		if err := dst.Create(&model.Setting{Key: k, Value: values[k]}).Error; err != nil {
			return err
		}
		fmt.Fprintf(&listing, "- %s = %s\n", k, values[k])
	}
	report.section("## 写入的设置\n\n%s\n> 订阅改为 0.0.0.0:2056 直出 HTTPS(原 nginx 对外口),路径与加密沿用旧值,用户订阅 URL 不变。", listing.String())
	return nil
}

func importLocalNode(src, dst *gorm.DB, report *Report) error {
	var domain string
	src.Raw("SELECT value FROM settings WHERE key = 'webDomain'").Scan(&domain)
	if domain == "" {
		report.warnf("旧库未设置 webDomain,本机节点域名留空,请在面板补填")
	}
	node := model.Node{
		Name:    "香港",
		Domain:  domain,
		IsLocal: true,
		Token:   randomToken(),
		Sort:    1,
	}
	if err := dst.Create(&node).Error; err != nil {
		return err
	}
	report.section("## 入口服务器\n\n创建本机节点 %q(domain=%s)。台湾节点在面板接入后自动加入。", node.Name, domain)
	return nil
}

func randomToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// ---- 排序文件 / 核对 / 报告 ----

func loadOrder(orderFile string) (map[string]int, error) {
	rank := map[string]int{}
	if orderFile == "" {
		return rank, nil
	}
	data, err := os.ReadFile(orderFile)
	if err != nil {
		return nil, fmt.Errorf("读取排序文件: %w", err)
	}
	i := 0
	for _, raw := range strings.Split(string(data), "\n") {
		name := strings.TrimSpace(raw)
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		if _, dup := rank[name]; !dup {
			rank[name] = i
			i++
		}
	}
	return rank, nil
}

// verify 用独立查询交叉核对关键守恒量:用户数、启用数、流量总和、配额总和。
func verify(src, dst *gorm.DB, report *Report) error {
	type sums struct {
		N       int64
		Enabled int64
		Up      int64
		Down    int64
		Volume  int64
	}
	var s, d sums
	if err := src.Raw(`SELECT COUNT(*) n, SUM(enable) enabled, COALESCE(SUM(up),0) up,
		COALESCE(SUM(down),0) down, COALESCE(SUM(volume),0) volume FROM clients`).Scan(&s).Error; err != nil {
		return err
	}
	if err := dst.Raw(`SELECT COUNT(*) n, SUM(enabled) enabled, COALESCE(SUM(up),0) up,
		COALESCE(SUM(down),0) down, COALESCE(SUM(volume),0) volume FROM users`).Scan(&d).Error; err != nil {
		return err
	}
	if s != d {
		return fmt.Errorf("核对失败:源 %+v != 目标 %+v", s, d)
	}
	report.section("## 核对\n\n用户数 %d、启用 %d、累计上行 %d、累计下行 %d、配额总和 %d —— 源库与目标库完全一致 ✅",
		d.N, d.Enabled, d.Up, d.Down, d.Volume)
	return nil
}

func writeReport(to string, report *Report) error {
	var b strings.Builder
	b.WriteString("# m-ui 导入报告\n\n")
	fmt.Fprintf(&b, "线路 %d 条,上游 %d 个,用户 %d 个(线路关系 %d 条)。\n\n",
		report.Lines, report.Upstreams, report.Users, report.UserLines)
	for _, s := range report.Sections {
		b.WriteString(s)
		b.WriteString("\n\n")
	}
	if len(report.Warnings) > 0 {
		b.WriteString("## 警告\n\n")
		for _, w := range report.Warnings {
			fmt.Fprintf(&b, "- ⚠️ %s\n", w)
		}
	} else {
		b.WriteString("## 警告\n\n无。\n")
	}
	path := strings.TrimSuffix(to, ".db") + "-report.md"
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return err
	}
	fmt.Println(b.String())
	fmt.Println("报告已写入:", path)
	return nil
}

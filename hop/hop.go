// Package hop 实现 Hysteria2 端口跳跃:在本机用 nftables(回落 iptables)把一段 UDP 端口
// DNAT/REDIRECT 到线路真实端口。客户端在范围内随机换端口,规避运营商对单端口 UDP 的限速/封锁。
package hop

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// Rule 一条端口跳跃规则:UDP From-To → Port。
type Rule struct {
	From, To, Port int
}

var reRange = regexp.MustCompile(`^\s*(\d{1,5})\s*[-:]\s*(\d{1,5})\s*$`)

// ParseRange 解析 "20000-30000" 这样的范围并校验。
func ParseRange(s string) (int, int, error) {
	m := reRange.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, errors.New("端口范围格式应为 起始-结束,如 20000-30000")
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	if a < 1024 || b > 65535 || a >= b {
		return 0, 0, errors.New("端口范围需在 1024–65535 之间且起始小于结束")
	}
	if b-a < 10 {
		return 0, 0, errors.New("端口范围太小,至少 10 个端口")
	}
	return a, b, nil
}

// Normalize 返回规范化的 "a-b" 文本。
func Normalize(s string) (string, error) {
	a, b, err := ParseRange(s)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d-%d", a, b), nil
}

// Overlaps 检查规则之间是否有范围重叠或覆盖了其它线路端口。
func Overlaps(rules []Rule, ports []int) error {
	for i, r := range rules {
		for j, o := range rules {
			if i < j && r.From <= o.To && o.From <= r.To {
				return fmt.Errorf("端口跳跃范围 %d-%d 与 %d-%d 重叠", r.From, r.To, o.From, o.To)
			}
		}
		for _, p := range ports {
			if p != r.Port && p >= r.From && p <= r.To {
				return fmt.Errorf("端口跳跃范围 %d-%d 覆盖了其它线路的端口 %d", r.From, r.To, p)
			}
		}
	}
	return nil
}

// NFTScript 生成整表替换的 nft 脚本(幂等)。
func NFTScript(rules []Rule) string {
	var b strings.Builder
	b.WriteString("table inet m_ui_hop\ndelete table inet m_ui_hop\n")
	if len(rules) == 0 {
		return b.String()
	}
	b.WriteString("table inet m_ui_hop {\n\tchain prerouting {\n\t\ttype nat hook prerouting priority dstnat; policy accept;\n")
	for _, r := range rules {
		fmt.Fprintf(&b, "\t\tudp dport %d-%d redirect to :%d\n", r.From, r.To, r.Port)
	}
	b.WriteString("\t}\n}\n")
	return b.String()
}

// Apply 在本机应用规则(Linux root);非 Linux 或无权限时静默跳过并返回 nil。
// 优先 nftables;没有 nft 时回落 iptables/ip6tables 自建链 M_UI_HOP。
func Apply(rules []Rule) error {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return nil
	}
	if _, err := exec.LookPath("nft"); err == nil {
		cmd := exec.Command("nft", "-f", "-")
		cmd.Stdin = strings.NewReader(NFTScript(rules))
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			// "delete table" 在表不存在时会失败 → 先建再删的脚本已规避;其它错误如实返回
			return fmt.Errorf("nft 应用失败: %v: %s", err, strings.TrimSpace(out.String()))
		}
		return nil
	}
	return applyIptables(rules)
}

func applyIptables(rules []Rule) error {
	for _, bin := range []string{"iptables", "ip6tables"} {
		if _, err := exec.LookPath(bin); err != nil {
			continue
		}
		run := func(args ...string) error {
			c := exec.Command(bin, args...)
			var out bytes.Buffer
			c.Stdout, c.Stderr = &out, &out
			if err := c.Run(); err != nil {
				return fmt.Errorf("%s %s: %s", bin, strings.Join(args, " "), strings.TrimSpace(out.String()))
			}
			return nil
		}
		_ = run("-t", "nat", "-N", "M_UI_HOP")
		if err := run("-t", "nat", "-F", "M_UI_HOP"); err != nil {
			return err
		}
		// 确保 PREROUTING 跳到自建链(只加一次)
		if err := run("-t", "nat", "-C", "PREROUTING", "-j", "M_UI_HOP"); err != nil {
			if err := run("-t", "nat", "-I", "PREROUTING", "-j", "M_UI_HOP"); err != nil {
				return err
			}
		}
		for _, r := range rules {
			if err := run("-t", "nat", "-A", "M_UI_HOP", "-p", "udp", "--dport", fmt.Sprintf("%d:%d", r.From, r.To), "-j", "REDIRECT", "--to-ports", strconv.Itoa(r.Port)); err != nil {
				return err
			}
		}
	}
	return nil
}

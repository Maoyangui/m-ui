// Package portprobe 试着监听一个端口,看它是不是被这台机器上别的程序占着;占着的话查出是哪个程序。
//
// 保存线路 / 中转入口时由要开端口的那台服务器自己探(本机直接调,副机经 agent/port-check);
// 数据面摘掉「端口被占」的入站之后,也用它每分钟看一次端口空出来没有。
package portprobe

import (
	"context"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// SelfName ss 里看到的本程序的进程名:占着端口的是它,就不算「被别的程序占用」。
const SelfName = "m-ui"

// Probe 探一个端口:Net 为 tcp / udp 只探那一种,空 = 两种都要能监听才算空闲。
func Probe(p model.PortProbe) model.PortProbeResult {
	res := model.PortProbeResult{Port: p.Port, Net: p.Net, Free: true}
	if p.Port < 1 || p.Port > 65535 {
		res.Free = false
		return res
	}
	addr := ":" + strconv.Itoa(p.Port)
	busy := func(network string) {
		res.Free = false
		res.Net = network
		res.By = Holder(p.Port, network)
		res.Self = res.By == SelfName
	}
	if p.Net != "udp" {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			busy("tcp")
			return res
		}
		ln.Close()
	}
	if p.Net != "tcp" {
		pc, err := net.ListenPacket("udp", addr)
		if err != nil {
			busy("udp")
			return res
		}
		pc.Close()
	}
	return res
}

// Free 端口现在能不能监听(network 同 Probe 的 Net)。
func Free(port int, network string) bool {
	return Probe(model.PortProbe{Port: port, Net: network}).Free
}

var holderRe = regexp.MustCompile(`users:\(\("([^"]+)"`)

// Holder 占着这个端口的进程名(Linux 上用 ss 查;没有 ss、查不到、别的系统都返回空)。
func Holder(port int, network string) string {
	flag := "-ltnp"
	if network == "udp" {
		flag = "-lunp"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ss", "-H", flag, "sport = :"+strconv.Itoa(port)).Output()
	if err != nil {
		return ""
	}
	return parseHolder(string(out))
}

// parseHolder 从 ss 的输出里取第一个进程名(users:(("realm",pid=812,fd=9)) → realm)。
func parseHolder(out string) string {
	m := holderRe.FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

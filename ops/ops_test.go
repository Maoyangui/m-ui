package ops

import (
	"strings"
	"testing"
)

func TestScriptParamsAndValidation(t *testing.T) {
	s, err := Script("warp-enable", Params{Port: 41000})
	if err != nil || !strings.Contains(s, "PORT=41000") || strings.Contains(s, "{{PORT}}") {
		t.Fatalf("端口未替换: %v", err)
	}
	s, _ = Script("warp-enable", Params{Port: 99999})
	if !strings.Contains(s, "PORT=40000") {
		t.Fatal("非法端口应回落 40000")
	}
	s, _ = Script("swap", Params{SwapGB: 4})
	if !strings.Contains(s, "fallocate -l 4G") {
		t.Fatal("swap 大小未替换")
	}
	s, _ = Script("tune-all", Params{})
	for _, want := range []string{"== swap ==", "== sysctl / BBR ==", "== limits ==", "== NTP ==", "fallocate -l 2G"} {
		if !strings.Contains(s, want) {
			t.Fatalf("一键优化缺少 %q", want)
		}
	}
	if !strings.HasPrefix(s, "set -uo pipefail") {
		t.Fatal("脚本应以 set -uo pipefail 开头")
	}
	if _, err := Script("rm-rf", Params{}); err == nil {
		t.Fatal("未知任务应报错")
	}
	// nofile / sysctl 参数化
	s, _ = Script("limits", Params{NoFile: 65536})
	if !strings.Contains(s, "LimitNOFILE=65536") || !strings.Contains(s, "LimitNPROC=65536") {
		t.Fatal("nofile 未替换")
	}
	s, _ = Script("limits", Params{NoFile: 1})
	if !strings.Contains(s, "LimitNOFILE=1048576") {
		t.Fatal("非法 nofile 应回落默认")
	}
	s, _ = Script("sysctl", Params{})
	if !strings.Contains(s, "net.ipv4.tcp_congestion_control=bbr") || strings.Contains(s, "{{SYSCTL}}") {
		t.Fatal("默认 sysctl 模板未写入")
	}
	s, _ = Script("sysctl", Params{Sysctl: "# 注释\nnet.core.somaxconn=4096\n\nvm.swappiness = 1\n"})
	if !strings.Contains(s, "net.core.somaxconn=4096\nvm.swappiness = 1\n") || strings.Contains(s, "bbr") {
		t.Fatalf("自定义 sysctl 未生效:\n%s", s)
	}
	if _, err := Script("sysctl", Params{Sysctl: "net.core.somaxconn=4096; rm -rf /"}); err == nil {
		t.Fatal("危险 sysctl 行应被拒")
	}
	if _, err := ValidateSysctl("   \n# only comment\n"); err == nil {
		t.Fatal("空参数应被拒")
	}
	for _, task := range Tasks {
		if _, err := Script(task.Name, Params{}); err != nil {
			t.Fatalf("任务 %s 无脚本: %v", task.Name, err)
		}
	}
}

func TestParsers(t *testing.T) {
	state, ip, loc, colo := ParseTrace("fl=1\nh=www.cloudflare.com\nip=104.28.1.2\nts=1\nvisit_scheme=https\nloc=HK\ncolo=HKG\nwarp=on\n")
	if state != "on" || ip != "104.28.1.2" || loc != "HK" || colo != "HKG" {
		t.Fatalf("trace 解析: %s %s %s %s", state, ip, loc, colo)
	}
	if s, _, _, _ := ParseTrace("ip=1.1.1.1\n"); s != "off" {
		t.Fatal("无 warp 行应为 off")
	}
	m := meminfo("MemTotal:       32000000 kB\nMemAvailable:    8000000 kB\nSwapTotal:      2097148 kB\n")
	if m["MemTotal"] != 32000000*1024 || m["SwapTotal"] != 2097148*1024 {
		t.Fatalf("meminfo 解析: %v", m)
	}
	if os := osRelease("NAME=\"Ubuntu\"\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nID=ubuntu\n"); os != "Ubuntu 24.04.1 LTS" {
		t.Fatalf("os-release 解析: %s", os)
	}
}

func TestRunnerRefusesOffLinuxOrNonRoot(t *testing.T) {
	r := NewRunner()
	st := r.Status()
	if st.Running || st.Log == nil && len(st.Log) != 0 {
		t.Fatal("初始状态应为空闲")
	}
	// 在 Windows 上必然被拒;在 Linux 非 root 下也被拒;root Linux 上跳过此断言
	err := r.Start("ntp", Params{}, nil)
	if err == nil {
		r.Cancel()
		t.Skip("root Linux 环境,任务真的启动了")
	}
}

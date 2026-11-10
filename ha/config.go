package ha

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/logger"
)

// Config 主机容灾的设置(全局设置,随副本复制,接任后的主机照用)。Cloudflare 令牌只说有没有设,从不回显。
type Config struct {
	Mode            string `json:"mode"`     // off | confirm | auto
	Standbys        int    `json:"standbys"` // 备用台数
	ReplicaMinutes  int    `json:"replicaMinutes"`
	ConfirmMinutes  int    `json:"confirmMinutes"`
	AutoMinutes     int    `json:"autoMinutes"`
	RankStepMinutes int    `json:"rankStepMinutes"`
	Witness         bool   `json:"witness"`
	PauseUntil      int64  `json:"pauseUntil"`
	DnsNames        string `json:"dnsNames"`
	DnsTokenSet     bool   `json:"dnsTokenSet"`
}

// 取值范围(界面的"高级"里能改;默认值开箱即用)。
const (
	minStandbys, maxStandbys     = 1, 3
	minReplicaMin, maxReplicaMin = 1, 60
	minConfirmMin, maxConfirmMin = 2, 30
	minAutoMin, maxAutoMin       = 5, 60
	minRankStep, maxRankStep     = 1, 10
)

// Config 读当前设置(没设的取默认值)。
func (s *Service) Config() Config {
	c := Config{
		Mode:            s.Mode(),
		Standbys:        s.settingInt(KeyStandbys, 2, minStandbys, maxStandbys),
		ReplicaMinutes:  s.settingInt(KeyReplicaMinutes, 5, minReplicaMin, maxReplicaMin),
		ConfirmMinutes:  s.settingInt(KeyConfirmMinutes, 3, minConfirmMin, maxConfirmMin),
		AutoMinutes:     s.settingInt(KeyAutoMinutes, 10, minAutoMin, maxAutoMin),
		RankStepMinutes: s.settingInt(KeyRankStepMinutes, 3, minRankStep, maxRankStep),
		Witness:         s.settingBool(KeyWitness, true),
		DnsNames:        s.setting(KeyDnsNames),
		DnsTokenSet:     strings.TrimSpace(s.setting(KeyDnsToken)) != "",
	}
	c.PauseUntil, _ = strconv.ParseInt(s.setting(KeyPauseUntil), 10, 64)
	return c
}

// ConfigInput 保存设置的请求:DnsToken 非空 = 换成它;ClearDnsToken = 清掉。
type ConfigInput struct {
	Config
	DnsToken      string `json:"dnsToken"`
	ClearDnsToken bool   `json:"clearDnsToken"`
}

// SaveConfig 保存设置。第一次开启时本机纪元设为 1、生成签名密钥与集群密钥;关掉后纪元保留
// (副机已经认了纪元,不带纪元的请求会被当成旧主机),只是不再推副本、不再判定接任,备用删掉手里的副本。
func (s *Service) SaveConfig(in ConfigInput) error {
	if s.IsNode() {
		return errors.New("副机上不能改主机容灾:到主机面板改")
	}
	c := in.Config
	switch c.Mode {
	case ModeOff, ModeConfirm, ModeAuto:
	case "":
		c.Mode = ModeOff
	default:
		return fmt.Errorf("主机容灾只能是 off、confirm、auto,不是 %q", c.Mode)
	}
	if c.Standbys < minStandbys || c.Standbys > maxStandbys {
		return fmt.Errorf("备用台数要在 %d 到 %d 之间", minStandbys, maxStandbys)
	}
	if c.ReplicaMinutes < minReplicaMin || c.ReplicaMinutes > maxReplicaMin {
		return fmt.Errorf("数据副本间隔要在 %d 到 %d 分钟之间", minReplicaMin, maxReplicaMin)
	}
	if c.ConfirmMinutes < minConfirmMin || c.ConfirmMinutes > maxConfirmMin {
		return fmt.Errorf("确认提示的等待时间要在 %d 到 %d 分钟之间", minConfirmMin, maxConfirmMin)
	}
	if c.AutoMinutes < minAutoMin || c.AutoMinutes > maxAutoMin {
		return fmt.Errorf("自动接任的等待时间要在 %d 到 %d 分钟之间", minAutoMin, maxAutoMin)
	}
	if c.RankStepMinutes < minRankStep || c.RankStepMinutes > maxRankStep {
		return fmt.Errorf("备用之间的间隔要在 %d 到 %d 分钟之间", minRankStep, maxRankStep)
	}
	if c.Mode == ModeAuto && s.clusterSize() < 3 && !c.Witness {
		return errors.New("只有一主一副时,自动接任必须开着境外测点见证:否则主机只是和副机之间断网也会被接任")
	}
	names, err := normDnsNames(c.DnsNames)
	if err != nil {
		return err
	}
	values := map[string]string{
		KeyMode: c.Mode, KeyStandbys: strconv.Itoa(c.Standbys),
		KeyReplicaMinutes: strconv.Itoa(c.ReplicaMinutes), KeyConfirmMinutes: strconv.Itoa(c.ConfirmMinutes),
		KeyAutoMinutes: strconv.Itoa(c.AutoMinutes), KeyRankStepMinutes: strconv.Itoa(c.RankStepMinutes),
		KeyWitness: strconv.FormatBool(c.Witness), KeyDnsNames: names,
	}
	if tok := strings.TrimSpace(in.DnsToken); tok != "" {
		values[KeyDnsToken] = tok
	} else if in.ClearDnsToken {
		values[KeyDnsToken] = ""
	}
	first := false
	if c.Mode != ModeOff && s.Epoch() == 0 {
		first = true
		values[KeyEpoch] = "1"
		values[KeyMasterId] = strconv.FormatUint(uint64(s.SelfId()), 10)
	}
	if err := s.set(values); err != nil {
		return err
	}
	if first {
		if _, err := s.signKey(); err != nil {
			return err
		}
		s.clusterKey()
		logger.Info("主机容灾已开启:本机纪元 1")
	}
	s.invalidate()
	s.rep.kick()
	return nil
}

// normDnsNames 规整要跟着主机走的域名:逗号或空白分隔,去重,只收域名。
func normDnsNames(raw string) (string, error) {
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == '，' }) {
		f = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(f), "."))
		if f == "" || seen[f] {
			continue
		}
		if !validHostname(f) {
			return "", fmt.Errorf("「%s」不是域名:只填域名,不带 http:// 和端口", f)
		}
		seen[f] = true
		out = append(out, f)
	}
	if len(out) > 20 {
		return "", errors.New("要跟着主机走的域名最多 20 个")
	}
	return strings.Join(out, ","), nil
}

func validHostname(h string) bool {
	if len(h) > 253 || !strings.Contains(h, ".") {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i, r := range label {
			ok := r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (r == '*' && i == 0 && len(label) == 1)
			if !ok {
				return false
			}
		}
	}
	return true
}

// Pause 暂停接任 minutes 分钟(0 = 恢复)。在主机上点、命令行、升级前自动调用;马上推给备用。
func (s *Service) Pause(minutes int) error {
	if s.IsNode() {
		return errors.New("副机上不能暂停:到主机面板操作")
	}
	if minutes < 0 || minutes > 7*24*60 {
		return errors.New("暂停时长要在 0 到 7 天之间")
	}
	v := ""
	if minutes > 0 {
		v = strconv.FormatInt(s.now().Add(time.Duration(minutes)*time.Minute).Unix(), 10)
	}
	if err := s.set(map[string]string{KeyPauseUntil: v}); err != nil {
		return err
	}
	s.invalidate()
	s.pushStandbysSoon()
	return nil
}

// PauseForUpgrade 面板一键升级前暂停 15 分钟:升级重启那一两分钟备用别误判主机失联。没开容灾什么都不做。
func (s *Service) PauseForUpgrade() { s.pauseAtLeast(15*time.Minute, "升级前") }

// PauseForRestart 主机计划内重启(改端口、还原备份……)前暂停 5 分钟。
func (s *Service) PauseForRestart() { s.pauseAtLeast(5*time.Minute, "重启前") }

func (s *Service) pauseAtLeast(d time.Duration, why string) {
	if s.Epoch() == 0 || s.IsNode() || s.Mode() == ModeOff {
		return
	}
	cur, _ := strconv.ParseInt(s.setting(KeyPauseUntil), 10, 64)
	until := s.now().Add(d).Unix()
	if cur >= until {
		return
	}
	if err := s.set(map[string]string{KeyPauseUntil: strconv.FormatInt(until, 10)}); err != nil {
		logger.Warning("主机容灾:", why, "暂停接任失败: ", err)
		return
	}
	s.invalidate()
	s.pushStandbys(5 * time.Second) // 重启就在眼前:等不到 hub 的下一轮
	logger.Info("主机容灾:", why, "暂停接任 ", int(d.Minutes()), " 分钟")
}

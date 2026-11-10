package ha

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/logger"
)

// Config 容灾配置(全局设置,随副本复制,接管后临时主机照用)。
type Config struct {
	Mode            string `json:"mode"`
	Candidates      int    `json:"candidates"`
	Exclude         []uint `json:"exclude"`
	ReplicaMinutes  int    `json:"replicaMinutes"`
	ConfirmMinutes  int    `json:"confirmMinutes"`
	AutoMinutes     int    `json:"autoMinutes"`
	RankStepMinutes int    `json:"rankStepMinutes"`
	Witness         bool   `json:"witness"`
	Recovery        bool   `json:"recovery"`
	IncludeCerts    bool   `json:"includeCerts"`
	DisableFormer   bool   `json:"disableFormer"`
	PauseUntil      int64  `json:"pauseUntil"`
}

// 取值范围。
const (
	minCandidates, maxCandidates = 1, 3
	minReplicaMin, maxReplicaMin = 1, 60
	minConfirmMin, maxConfirmMin = 2, 30
	minAutoMin, maxAutoMin       = 5, 60
	minRankStep, maxRankStep     = 1, 10
)

// Config 读当前配置(没设的取默认值)。
func (s *Service) Config() Config {
	c := Config{
		Mode:            s.Mode(),
		Candidates:      s.settingInt(KeyCandidates, 2, minCandidates, maxCandidates),
		ReplicaMinutes:  s.settingInt(KeyReplicaMinutes, 5, minReplicaMin, maxReplicaMin),
		ConfirmMinutes:  s.settingInt(KeyConfirmMinutes, 3, minConfirmMin, maxConfirmMin),
		AutoMinutes:     s.settingInt(KeyAutoMinutes, 10, minAutoMin, maxAutoMin),
		RankStepMinutes: s.settingInt(KeyRankStepMinutes, 3, minRankStep, maxRankStep),
		Witness:         s.settingBool(KeyWitness, true),
		Recovery:        s.settingBool(KeyRecovery, false),
		IncludeCerts:    s.settingBool(KeyIncludeCerts, true),
		DisableFormer:   s.settingBool(KeyDisableFormer, true),
	}
	c.PauseUntil, _ = strconv.ParseInt(s.setting(KeyPauseUntil), 10, 64)
	_ = json.Unmarshal([]byte(s.setting(KeyExclude)), &c.Exclude)
	if c.Exclude == nil {
		c.Exclude = []uint{}
	}
	return c
}

// SaveConfig 保存配置。第一次打开容灾时本机纪元设为 1、生成签名密钥与集群密钥;关掉后纪元保留
// (副机已经认了纪元,不带纪元的请求会被当成旧主机),只是不再推副本、不再判定接管,候选删掉手里的副本。
func (s *Service) SaveConfig(c Config) error {
	if s.IsNode() {
		return errors.New("副机上不能改容灾配置:到主机面板改")
	}
	switch c.Mode {
	case ModeOff, ModeManual, ModeConfirm, ModeAuto:
	case "":
		c.Mode = ModeOff
	default:
		return fmt.Errorf("容灾模式只能是 off、manual、confirm、auto,不是 %q", c.Mode)
	}
	if c.Candidates < minCandidates || c.Candidates > maxCandidates {
		return fmt.Errorf("候选台数要在 %d 到 %d 之间", minCandidates, maxCandidates)
	}
	if c.ReplicaMinutes < minReplicaMin || c.ReplicaMinutes > maxReplicaMin {
		return fmt.Errorf("副本间隔要在 %d 到 %d 分钟之间", minReplicaMin, maxReplicaMin)
	}
	if c.ConfirmMinutes < minConfirmMin || c.ConfirmMinutes > maxConfirmMin {
		return fmt.Errorf("确认提示的等待时间要在 %d 到 %d 分钟之间", minConfirmMin, maxConfirmMin)
	}
	if c.AutoMinutes < minAutoMin || c.AutoMinutes > maxAutoMin {
		return fmt.Errorf("自动接管的等待时间要在 %d 到 %d 分钟之间", minAutoMin, maxAutoMin)
	}
	if c.RankStepMinutes < minRankStep || c.RankStepMinutes > maxRankStep {
		return fmt.Errorf("名次间隔要在 %d 到 %d 分钟之间", minRankStep, maxRankStep)
	}
	if c.Recovery && c.Mode != ModeManual && c.Mode != ModeOff {
		return errors.New("恢复码模式下候选自己解不开副本,只能手动接管:模式请选「手动」")
	}
	if c.Recovery && strings.TrimSpace(s.setting(KeyRecoveryKEK)) == "" {
		return errors.New("先生成恢复码,再打开恢复码模式")
	}
	if c.Mode == ModeAuto && s.clusterSize() < 3 && !c.Witness {
		return errors.New("只有一主一副时,自动接管必须开着 Globalping 见证:否则主机只是和副机之间断网也会被接管")
	}
	ex, _ := json.Marshal(c.Exclude)
	if c.Exclude == nil {
		ex = []byte("[]")
	}
	values := map[string]string{
		KeyMode: c.Mode, KeyCandidates: strconv.Itoa(c.Candidates), KeyExclude: string(ex),
		KeyReplicaMinutes: strconv.Itoa(c.ReplicaMinutes), KeyConfirmMinutes: strconv.Itoa(c.ConfirmMinutes),
		KeyAutoMinutes: strconv.Itoa(c.AutoMinutes), KeyRankStepMinutes: strconv.Itoa(c.RankStepMinutes),
		KeyWitness: strconv.FormatBool(c.Witness), KeyRecovery: strconv.FormatBool(c.Recovery),
		KeyIncludeCerts: strconv.FormatBool(c.IncludeCerts), KeyDisableFormer: strconv.FormatBool(c.DisableFormer),
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
		logger.Info("容灾已开启:本机纪元 1")
	}
	s.invalidate()
	s.rep.kick()
	return nil
}

// Pause 暂停自动接管 minutes 分钟(0 = 取消暂停)。主机上点、升级前自动调用;随容灾状态推给候选。
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
	return nil
}

// PauseForUpgrade 面板一键升级前自动暂停 15 分钟:升级重启那一两分钟候选别误判主机失联。没开容灾什么都不做。
func (s *Service) PauseForUpgrade() {
	if s.Epoch() == 0 || s.IsNode() || s.Mode() == ModeOff {
		return
	}
	cur, _ := strconv.ParseInt(s.setting(KeyPauseUntil), 10, 64)
	until := s.now().Add(15 * time.Minute).Unix()
	if cur >= until {
		return
	}
	if err := s.set(map[string]string{KeyPauseUntil: strconv.FormatInt(until, 10)}); err != nil {
		logger.Warning("升级前暂停自动接管失败: ", err)
		return
	}
	s.invalidate() // 下一轮(5 秒内)就把暂停推给候选:调用方在下载新版本之前就调它
	logger.Info("容灾:升级前暂停自动接管 15 分钟")
}

// PromotedFrom 临时主机的接管记录。
type PromotedFrom struct {
	FormerId    uint   `json:"formerId"`
	FormerName  string `json:"formerName"`
	Epoch       uint64 `json:"epoch"` // 接管后的纪元
	Seq         uint64 `json:"seq"`
	ReplicaAt   int64  `json:"replicaAt"`
	MaxChangeId uint64 `json:"maxChangeId"`
	At          int64  `json:"at"`
	Force       bool   `json:"force,omitempty"`
}

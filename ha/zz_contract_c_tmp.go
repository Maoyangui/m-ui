package ha

// 临时适配(修复者 C):设计 3.2 里归 A / B 的契约函数与类型,C 开工时 A0 还没合入,这里按契约签名先给一份
// 保持现状的桩,C 的代码只经这些名字调用。**集成时整个文件删掉**:A0 / B 的同名定义会和这里重复,编译器会指出来;
// 另把 cmd.go 里的 s.setMasterWith( 换成 s.SetMaster(。

import (
	"os"
	"path/filepath"
	"strconv"

	"github.com/Maoyangui/m-ui/logger"

	"gorm.io/gorm"
)

// takeoverVerdict 契约 A:A0 的桩为 (true, "")。
func (s *Service) takeoverVerdict() (bool, string) { return true, "" }

// lastTakeoverErr 契约 A:本机最近一次接任失败的时间(给同伴看)。
func (s *Service) lastTakeoverErr() int64 { return 0 }

// ReclaimNow 契约 A:A0 的桩为 s.reclaim(0, true)。
func (s *Service) ReclaimNow() (uint64, error) { return s.reclaim(0, true) }

// SignPub 契约 A:本机签名公钥(= signKey)。
func (s *Service) SignPub() (string, error) { return s.signKey() }

// EnterEpoch 契约 A:纪元 0 → 1(原 config.go 首次开启那段)。
func (s *Service) EnterEpoch() (uint64, error) {
	if e := s.Epoch(); e > 0 {
		return e, nil
	}
	if err := s.set(map[string]string{KeyEpoch: "1", KeyMasterId: strconv.FormatUint(uint64(s.SelfId()), 10)}); err != nil {
		return 0, err
	}
	if _, err := s.signKey(); err != nil {
		return 0, err
	}
	s.clusterKey()
	logger.Info("主机容灾已开启:本机纪元 1")
	return 1, nil
}

// ResetLocal 契约 A(A0 即真实现):清本机的容灾键、副本文件、standby.key、epoch.json。这里只是 C 的命令行能编译、能用的近似。
func ResetLocal(db *gorm.DB, dataDir string) error {
	keys := []string{KeyEpoch, KeyRole, KeyMasterId, KeyMasterPub, KeyVote, KeyGrant, KeyState, KeyNotify, KeyPeerKey,
		KeyDemoted, KeyFreezeUntil, KeySwitch, KeyIncoming, KeyBecame, KeySignKey, KeyStandbyList, KeyDropPending}
	if err := db.Transaction(func(tx *gorm.DB) error {
		values := map[string]string{}
		for _, k := range keys {
			values[k] = ""
		}
		return upsert(tx, values)
	}); err != nil {
		return err
	}
	dir := HADir(dataDir)
	if err := dropStandbyFiles(dir, true); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, "epoch.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// SetMasterOpts 契约 B。
type SetMasterOpts struct{ AllowPlain bool }

// setMasterWith 契约 B 的 SetMaster(id, actor, o) 落地前的转接。
func (s *Service) setMasterWith(id uint, actor string, o SetMasterOpts) (SwitchState, error) {
	_ = o
	return s.SetMaster(id, actor)
}

// Instance 契约 A(boot.go):本机实例号。A0 落地前为空。
func (s *Service) Instance() string { return "" }

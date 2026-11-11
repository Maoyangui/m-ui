package ha

// 临时适配(修复者 B):修复设计 3.2「契约提交 A0」里本分支要调用、A 还没合进来的函数,按契约的签名与语义写的最小实现,
// 只为让 B 的改动能编译、能测。集成时整个文件删掉,一律以 A 的实现为准(A 的文件里为契约加的字段同理,见提交说明)。

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

// 纪元的上限与一次能跳多少(契约 A0:ha/epoch.go)。
const (
	MaxEpoch     uint64 = 1 << 40
	MaxEpochJump uint64 = 1000
)

// EpochClaimOK 别人报来的纪元可不可信(契约 A0)。
func EpochClaimOK(local, claim uint64) bool {
	return claim < MaxEpoch && claim <= local+MaxEpochJump
}

// NextEpoch max(local, 采信的 seen)+1,到上限返回错误(契约 A0)。
func NextEpoch(local uint64, seen ...uint64) (uint64, error) {
	m := local
	for _, e := range seen {
		if EpochClaimOK(local, e) && e > m {
			m = e
		}
	}
	n := m + 1
	if n >= MaxEpoch || n <= local {
		return 0, errors.New("纪元已到上限")
	}
	return n, nil
}

// SignPub 本机签名公钥(契约 A0:= signKey)。
func (s *Service) SignPub() (string, error) { return s.signKey() }

// EnterEpoch 纪元 0 → 1,模式不变(契约 A0:config.go 首次开启那段)。
func (s *Service) EnterEpoch() (uint64, error) {
	if s.IsNode() {
		return 0, errors.New("本机不是主机")
	}
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
	s.invalidate()
	return 1, nil
}

// beginDemote 置 demoting;已在降级返回 false(契约 A0)。
func (s *Service) beginDemote() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.demoting {
		return false
	}
	s.demoting = true
	return true
}

// FenceAbandon 设为主机结果不明时以更高纪元隔离目标:先推带自交接的状态(撤销由 switcher.revoke 带上),再升本机纪元
// (契约 A0 是"未实现"的桩;这里按设计 2.4 第 9 步写最小实现,B 的测试要走通)。
func (s *Service) FenceAbandon(target uint, targetEpoch uint64) (uint64, error) {
	if s.IsNode() {
		return 0, errors.New("本机不是主机")
	}
	L := s.Epoch()
	next, err := NextEpoch(L, targetEpoch)
	if err != nil {
		return 0, err
	}
	pub, err := s.signKey()
	if err != nil {
		return 0, err
	}
	self := s.SelfId()
	now := s.now()
	s.setGrantOut(&Grant{Epoch: next, To: self, ToPub: pub, From: self, FromEpoch: L, Expires: now.Add(10 * time.Minute).Unix(), At: now.Unix()})
	defer s.setGrantOut(nil)
	s.invalidate()
	s.pushAll(s.remoteNodes(), 15*time.Second)
	if err := s.set(map[string]string{KeyEpoch: strconv.FormatUint(next, 10), KeyMasterId: strconv.FormatUint(uint64(self), 10)}); err != nil {
		return 0, err
	}
	s.invalidate()
	return next, nil
}

// BootResult 启动判定(契约 A0:ha/boot.go)。
type BootResult struct {
	Fence    bool
	From, To uint64
	Reason   string
}

// Boot 启动时的纪元下限与换机围栏(契约 A0 的桩:返回零值)。
func (s *Service) Boot(restored bool) BootResult { return BootResult{} }

// ResetLocal 清掉本机的容灾状态:ha 开头的本机设置、副本文件、备用私钥(契约 A0 即真实现;服务不在跑也能用)。
func ResetLocal(db *gorm.DB, dataDir string) error {
	var keys []string
	if err := db.Raw("SELECT key FROM settings").Scan(&keys).Error; err != nil {
		return err
	}
	for _, k := range keys {
		if strings.HasPrefix(k, "ha") && IsLocal(k) {
			if err := db.Exec("DELETE FROM settings WHERE key = ?", k).Error; err != nil {
				return err
			}
		}
	}
	dir := HADir(dataDir)
	for _, n := range []string{fileCur, filePrev, fileTmp, fileInfo, fileKey} {
		_ = os.Remove(filepath.Join(dir, n))
	}
	return nil
}

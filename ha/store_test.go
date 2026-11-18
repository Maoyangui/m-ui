package ha

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// standbyPair 一台开着容灾的主机(纪元 1)和一台把它当主机的备用。
func standbyPair(t *testing.T) (*Service, *Service) {
	t.Helper()
	m := testService(t, false, map[string]string{KeyEpoch: "1", KeyMode: ModeConfirm})
	seedMaster(t, m)
	m.set(map[string]string{KeySignKey: ""})
	pub, err := m.signKey()
	if err != nil {
		t.Fatal(err)
	}
	c := testService(t, true, map[string]string{})
	c.d.DB.Create(&model.Node{Id: 1, Name: "HK", Enabled: true, Sort: 1})
	c.d.DB.Create(&model.Node{Id: 2, Name: "TW", IsLocal: true, Enabled: true, Sort: 2})
	mid := m.SelfId()
	st := State{Epoch: 1, MasterId: mid, MasterName: "HK", MasterPub: pub, Self: 2, Mode: ModeConfirm, Standbys: []uint{2}}
	c.set(map[string]string{KeyEpoch: "1", KeyMasterId: strconv.FormatUint(uint64(mid), 10), KeyMasterPub: pub, KeyState: mustJSON(st)})
	return m, c
}

// replicaFor 主机生成一份副本、封给备用。
func replicaFor(t *testing.T, m, c *Service, seq uint64) []byte {
	t.Helper()
	dir := t.TempDir()
	zp, meta, err := m.buildReplica(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(zp)
	k, err := c.standbyKey()
	if err != nil {
		t.Fatal(err)
	}
	dataKey := NewDataKey()
	wrap, _ := SealTo(dataKey, k.Pub)
	sign, err := m.signer()
	if err != nil {
		t.Fatal(err)
	}
	hdr := EnvHeader{Kind: KindReplica, Epoch: m.Epoch(), Seq: seq, CreatedAt: meta.CreatedAt, MasterId: meta.MasterId, MasterName: meta.MasterName,
		Version: meta.Version, MaxChangeId: meta.MaxChangeId, Wrap: wrap, To: Fingerprint(k.PubString())}
	f, _ := os.Open(zp)
	defer f.Close()
	var buf bytes.Buffer
	if err := WriteEnvelope(&buf, hdr, dataKey, f, st.Size(), sign); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// 备用收副本:验签、核对纪元与主机、序号只增,两代轮换;写到一半断了旧的不动;不是封给本机的、来路不对的都拒;
// 自检解得开、跑得过 integrity_check;删除能删干净;报告里带公钥与副本信息。
func TestStandbyStore(t *testing.T) {
	m, c := standbyPair(t)
	r1 := replicaFor(t, m, c, 10)
	if seq, err := c.ReceiveReplica(bytes.NewReader(r1)); err != nil || seq != 10 {
		t.Fatalf("第一份应收下: %d %v", seq, err)
	}
	if _, err := c.ReceiveReplica(bytes.NewReader(r1)); err == nil {
		t.Fatal("同一序号再来一次应拒(重放)")
	}
	r2 := replicaFor(t, m, c, 11)
	if _, err := c.ReceiveReplica(bytes.NewReader(r2)); err != nil {
		t.Fatal(err)
	}
	cur, _ := os.ReadFile(filepath.Join(c.Dir(), fileCur))
	prev, _ := os.ReadFile(filepath.Join(c.Dir(), filePrev))
	if !bytes.Equal(cur, r2) || !bytes.Equal(prev, r1) {
		t.Fatal("两代轮换不对:cur 应是新的,prev 是上一份")
	}
	for _, f := range []string{fileCur, filePrev, fileKey, fileInfo} {
		if st, err := os.Stat(filepath.Join(c.Dir(), f)); err != nil || (runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0) {
			t.Fatalf("%s 要是 0600: %v %v", f, err, st)
		}
	}
	// 写到一半断了:cur 不变,临时文件删掉
	r3 := replicaFor(t, m, c, 12)
	if _, err := c.ReceiveReplica(io.LimitReader(bytes.NewReader(r3), int64(len(r3)/2))); err == nil {
		t.Fatal("截断的副本应拒")
	}
	if cur2, _ := os.ReadFile(filepath.Join(c.Dir(), fileCur)); !bytes.Equal(cur2, r2) {
		t.Fatal("收坏一份不能动已有的 cur")
	}
	if _, err := os.Stat(filepath.Join(c.Dir(), fileTmp)); err == nil {
		t.Fatal("临时文件要删掉")
	}
	// 别的主机签的:拒
	other := testService(t, false, map[string]string{KeyEpoch: "1", KeyMode: ModeConfirm})
	seedMaster(t, other)
	other.set(map[string]string{KeySignKey: ""})
	if _, err := c.ReceiveReplica(bytes.NewReader(replicaFor(t, other, c, 99))); err == nil {
		t.Fatal("不是当前主机签的副本应拒")
	}
	// 纪元不对:拒
	m.set(map[string]string{KeyEpoch: "2"})
	if _, err := c.ReceiveReplica(bytes.NewReader(replicaFor(t, m, c, 100))); err == nil || !strings.Contains(err.Error(), "纪元") {
		t.Fatalf("纪元对不上应拒: %v", err)
	}
	m.set(map[string]string{KeyEpoch: "1"})
	// 自检
	if err := c.SelfCheck(); err != nil {
		t.Fatalf("自检应通过: %v", err)
	}
	ri, ok := c.replicaInfo()
	if !ok || ri.VerifiedAt == 0 || ri.VerifyErr != "" || ri.Seq != 11 {
		t.Fatalf("自检结果要记下: %+v", ri)
	}
	rep := c.Report()
	if rep == nil || rep.Pub == "" || rep.Replica == nil || rep.Replica.Seq != 11 || rep.Replica.VerifiedAt == 0 {
		t.Fatalf("报告里要有公钥与副本信息: %+v", rep)
	}
	// 主机确认"截至"
	c.noteReplicaMark(ReplicaMark{Seq: 11, At: ri.CreatedAt + 600})
	if ri2, _ := c.replicaInfo(); ri2.AsOf != ri.CreatedAt+600 {
		t.Fatal("主机确认的截至时间要记下")
	}
	// 不在名单里:不收
	st, _ := c.NodeState()
	st.Standbys = nil
	c.set(map[string]string{KeyState: mustJSON(st)})
	if _, err := c.ReceiveReplica(bytes.NewReader(replicaFor(t, m, c, 200))); err == nil {
		t.Fatal("不在备用名单里不收副本")
	}
	// 删除
	if err := c.DropReplica(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{fileCur, filePrev, fileTmp, fileInfo} {
		if _, err := os.Stat(filepath.Join(c.Dir(), f)); err == nil {
			t.Fatalf("删除后 %s 还在", f)
		}
	}
	if _, err := os.Stat(filepath.Join(c.Dir(), fileKey)); err != nil {
		t.Fatal("备用密钥要留着(公钥不变,以后不用重新信任)")
	}
}

// 副本自检能发现坏库:把 cur 里的密文改坏 → 自检报错。
func TestStandbySelfCheckDetectsDamage(t *testing.T) {
	m, c := standbyPair(t)
	if _, err := c.ReceiveReplica(bytes.NewReader(replicaFor(t, m, c, 1))); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(c.Dir(), fileCur)
	b, _ := os.ReadFile(p)
	b[len(b)/2] ^= 1
	os.WriteFile(p, b, 0o600)
	if err := c.SelfCheck(); err == nil {
		t.Fatal("密文坏了自检应报错")
	}
	if ri, _ := c.replicaInfo(); ri.VerifyErr == "" {
		t.Fatal("自检失败要记下原因")
	}
}

// 审计 E2:0.8.0 换副本时在「cur 改名成 prev」和「新的改名成 cur」之间退出,只剩 prev。
// 现在 replicaInfo 让 prev 顶上(replica.json 记的正是它),自检检 prev,解副本也能用 prev —— 接管不会说没有副本。
func TestReplicaOnlyPrevLeftStillUsable(t *testing.T) {
	m, c := standbyPair(t)
	if _, err := c.ReceiveReplica(bytes.NewReader(replicaFor(t, m, c, 20))); err != nil {
		t.Fatal(err)
	}
	// 摆出旧版本中途退出的现场:prev 是现在这份,cur 没了,replica.json 还是这份的信息
	cur, prev := filepath.Join(c.Dir(), fileCur), filepath.Join(c.Dir(), filePrev)
	os.Remove(prev)
	if err := os.Rename(cur, prev); err != nil {
		t.Fatal(err)
	}
	ri, ok := c.replicaInfo()
	if !ok || ri.Seq != 20 {
		t.Fatalf("只剩 prev 时应让它顶上: ok=%v seq=%d", ok, ri.Seq)
	}
	if err := c.SelfCheck(); err != nil {
		t.Fatalf("自检应检 prev 并通过: %v", err)
	}
	if _, _, name, err := c.openReplica(t.TempDir()); err != nil || name != filePrev {
		t.Fatalf("应能用 prev 解开: %s %v", name, err)
	}
	// 下一份到了:正常放进 cur
	if _, err := c.ReceiveReplica(bytes.NewReader(replicaFor(t, m, c, 21))); err != nil {
		t.Fatal(err)
	}
	if ri, ok := c.replicaInfo(); !ok || ri.Seq != 21 {
		t.Fatalf("新的一份应成为 cur: %v %d", ok, ri.Seq)
	}
	if _, err := os.Stat(cur); err != nil {
		t.Fatal("cur 应在")
	}
}

// 正常换副本时 cur 一直在:prev 是上一份的硬链接(或改名),新副本原子替换 cur。
func TestReplicaRotationKeepsCurPresent(t *testing.T) {
	m, c := standbyPair(t)
	r1 := replicaFor(t, m, c, 30)
	if _, err := c.ReceiveReplica(bytes.NewReader(r1)); err != nil {
		t.Fatal(err)
	}
	r2 := replicaFor(t, m, c, 31)
	if _, err := c.ReceiveReplica(bytes.NewReader(r2)); err != nil {
		t.Fatal(err)
	}
	cur, _ := os.ReadFile(filepath.Join(c.Dir(), fileCur))
	prev, _ := os.ReadFile(filepath.Join(c.Dir(), filePrev))
	if !bytes.Equal(cur, r2) || !bytes.Equal(prev, r1) {
		t.Fatal("cur 应是新的、prev 是上一份")
	}
	// 留旧的之后、新副本换上去之前出事(进程被杀、磁盘出错):cur 必须还在、还能用来接管。
	// 以前先把 cur 改名成 prev,这时就只剩 prev,replicaInfo 说本机没有副本(审计 E2)
	renameReplica = func(string, string) error { return errors.New("模拟:新副本换上去之前出事") }
	defer func() { renameReplica = os.Rename }()
	if _, err := c.ReceiveReplica(bytes.NewReader(replicaFor(t, m, c, 32))); err == nil {
		t.Fatal("换不上去应报错")
	}
	if b, err := os.ReadFile(filepath.Join(c.Dir(), fileCur)); err != nil || !bytes.Equal(b, r2) {
		t.Fatalf("换不上去时 cur 应原样还在: %v", err)
	}
	if ri, ok := c.replicaInfo(); !ok || ri.Seq != 31 {
		t.Fatalf("换不上去时应仍用原来那份: ok=%v seq=%d", ok, ri.Seq)
	}
}

package ha

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func sealTestEnvelope(t *testing.T, plain []byte, to BoxKey, signSeed string) ([]byte, []byte) {
	t.Helper()
	key := NewDataKey()
	wrap, err := SealTo(key, to.Pub)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := SignKey(signSeed)
	var buf bytes.Buffer
	hdr := EnvHeader{Kind: KindReplica, Epoch: 3, Seq: 9, MasterId: 1, Version: "0.7.17", Wrap: wrap, To: Fingerprint(to.PubString())}
	if err := WriteEnvelope(&buf, hdr, key, bytes.NewReader(plain), int64(len(plain)), func(b []byte) []byte { return signEd25519(k, b) }); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), key
}

func openTestEnvelope(t *testing.T, raw []byte, me BoxKey) ([]byte, *Envelope, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "env")
	env, err := ReceiveEnvelope(bytes.NewReader(raw), io.Discard, 0)
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := OpenWith(env.Header.Wrap, me)
	if err != nil {
		return nil, env, err
	}
	var out bytes.Buffer
	if err := env.DecryptFile(p, key, &out); err != nil {
		return nil, env, err
	}
	return out.Bytes(), env, nil
}

// 信封:加密后能解开(空明文、正好整块、跨块都对);换钥匙解不开;改动任意 1 个字节,哈希、签名或 AEAD 校验失败。
func TestEnvelopeRoundTripAndTamper(t *testing.T) {
	me, _ := NewBoxKey()
	other, _ := NewBoxKey()
	seed := NewSignKey()
	k, _ := SignKey(seed)
	pub, _ := ParseSignPub(PubOf(k))
	for _, n := range []int{0, 1, chunkSize, chunkSize + 1, 3*chunkSize + 17} {
		plain := randBytes(n)
		raw, _ := sealTestEnvelope(t, plain, me, seed)
		got, env, err := openTestEnvelope(t, raw, me)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("%d 字节:解不开或内容不对: %v", n, err)
		}
		if err := env.VerifySig(pub); err != nil {
			t.Fatalf("%d 字节:签名应通过: %v", n, err)
		}
		if _, _, err := openTestEnvelope(t, raw, other); err == nil {
			t.Fatalf("%d 字节:换一把钥匙不该解得开", n)
		}
	}
	plain := randBytes(2*chunkSize + 5)
	raw, _ := sealTestEnvelope(t, plain, me, seed)
	// 逐段各改一个字节:头部、密文头、密文中段、密文尾、哈希、签名
	hdrLen := len(envMagic) + 4
	for _, at := range []int{hdrLen + 3, hdrLen + 200, len(raw) / 2, len(raw) - 120, len(raw) - 70, len(raw) - 5} {
		bad := append([]byte(nil), raw...)
		bad[at] ^= 0x40
		got, env, err := openTestEnvelope(t, bad, me)
		if err == nil && env != nil && env.VerifySig(pub) == nil && bytes.Equal(got, plain) {
			t.Fatalf("改了第 %d 个字节,竟然哈希、签名、解密全都通过", at)
		}
	}
	// 截断
	if _, err := ReceiveEnvelope(bytes.NewReader(raw[:len(raw)-1]), io.Discard, 0); err == nil {
		t.Fatal("截断的信封应被拒")
	}
	// 尾巴多东西
	if _, err := ReceiveEnvelope(bytes.NewReader(append(append([]byte(nil), raw...), 0)), io.Discard, 0); err == nil {
		t.Fatal("信封后面多出数据应被拒")
	}
	// 别人的签名公钥验不过
	k2, _ := SignKey(NewSignKey())
	pub2, _ := ParseSignPub(PubOf(k2))
	_, env, _ := openTestEnvelope(t, raw, me)
	if err := env.VerifySig(pub2); err == nil {
		t.Fatal("不是当前主机签的应验签失败")
	}
	// 上限
	if _, err := ReceiveEnvelope(bytes.NewReader(raw), io.Discard, 100); err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("超出上限应被拒: %v", err)
	}
}

// 先加密成密文再按候选各拼头部(WriteEnvelopeCT)与一次写成(WriteEnvelope)结果逐字节相同;EnvelopeSize 算得准。
func TestEnvelopeCTMatches(t *testing.T) {
	plain := randBytes(chunkSize*2 + 99)
	key := NewDataKey()
	hdr := EnvHeader{Kind: KindReplica, Epoch: 1, Seq: 2, Wrap: "w", Size: int64(len(plain))}
	k, _ := SignKey(NewSignKey())
	sign := func(b []byte) []byte { return signEd25519(k, b) }
	var a bytes.Buffer
	if err := WriteEnvelope(&a, hdr, key, bytes.NewReader(plain), int64(len(plain)), sign); err != nil {
		t.Fatal(err)
	}
	var ct bytes.Buffer
	sum, err := SealStream(&ct, key, bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := WriteEnvelopeCT(&b, hdr, bytes.NewReader(ct.Bytes()), sum, sign); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("两种写法的信封应逐字节相同")
	}
	n, err := EnvelopeSize(hdr, 64)
	if err != nil || n != int64(b.Len()) {
		t.Fatalf("EnvelopeSize = %d,实际 %d (%v)", n, b.Len(), err)
	}
}

package ha

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/nacl/box"
)

// 副本与设为主机时整库数据的信封。只用 go.mod 里已有的 x/crypto 与标准库,不自己发明原语:
//
//	"MUIHA1\n" | uint32 头部长度 | 头部 JSON | 密文 | 32 字节密文 sha256 | uint16 签名长度 | 签名
//
//   - 正文用随机的 32 字节数据密钥,按 64 KiB 分块 ChaCha20-Poly1305 加密(块序号 + "最后一块"标志做 nonce,
//     截断、换序、拼接都过不了校验);
//   - 数据密钥包给收件人(nacl/box 匿名密封):副本用备用的长期 X25519 公钥;设为主机时用目标机器这一次临时生成的公钥
//     (经现有的服务器链接取得:令牌鉴权 + 钉住的证书指纹;链接是明文 http 时被动窃听者也解不开);
//   - 主机用 ed25519 签名(签头部 + 密文哈希),备用只认当前纪元主机的公钥 —— 防止有人伪造副本,
//     在接任时把自己的管理员带进去。
//
// 尾部放密文哈希与签名,收发两边都只过一遍数据:收的一方边收边写盘边算哈希,读到尾部再验签,不对就删。

const (
	envMagic   = "MUIHA1\n"
	chunkSize  = 64 << 10
	maxHeader  = 64 << 10
	sigContext = "m-ui-ha-envelope\x00"
)

// 信封种类。
const (
	KindReplica = "replica" // 给备用的数据副本
	KindBundle  = "bundle"  // 设为主机时推给目标机器的整库数据
)

// EnvHeader 信封头部(签名覆盖它)。
type EnvHeader struct {
	V           int    `json:"v"`
	Kind        string `json:"kind"`
	Epoch       uint64 `json:"epoch,omitempty"`
	Seq         uint64 `json:"seq,omitempty"`
	CreatedAt   int64  `json:"createdAt"`
	MasterId    uint   `json:"masterId,omitempty"`
	MasterName  string `json:"masterName,omitempty"`
	Version     string `json:"version"`
	Size        int64  `json:"size"`  // 明文字节数
	CtLen       int64  `json:"ctLen"` // 密文字节数
	MaxChangeId uint64 `json:"maxChangeId,omitempty"`
	Wrap        string `json:"wrap"`         // 包好的数据密钥(base64)
	To          string `json:"to,omitempty"` // 收件人公钥指纹
	Nonce       string `json:"nonce,omitempty"`
}

// Envelope 读出来的信封。
type Envelope struct {
	Header EnvHeader
	raw    []byte // 头部原文(签名对象的一部分)
	CtSum  [32]byte
	Sig    []byte
	ctOff  int64
}

// ctLenFor 明文 n 字节加密后的长度:每块多 16 字节认证标签,至少一块(空明文也有一个"最后一块")。
func ctLenFor(n int64) int64 {
	chunks := n / chunkSize
	if n%chunkSize != 0 || n == 0 {
		chunks++
	}
	return n + chunks*chacha20poly1305.Overhead
}

func chunkNonce(i uint64, last bool) []byte {
	nonce := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint64(nonce[3:11], i)
	if last {
		nonce[11] = 1
	}
	return nonce
}

// NewDataKey 一次性的数据密钥。
func NewDataKey() []byte {
	k := make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	return k
}

// WriteEnvelope 把 size 字节的明文 plain 加密写进 w。hdr 的 Size / CtLen 由这里填;sign 为 nil 时不签名。
func WriteEnvelope(w io.Writer, hdr EnvHeader, key []byte, plain io.Reader, size int64, sign func([]byte) []byte) error {
	hdr.V, hdr.Size, hdr.CtLen = 1, size, ctLenFor(size)
	raw, err := json.Marshal(hdr)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(w, 128<<10)
	if err := writeEnvHead(bw, raw); err != nil {
		return err
	}
	sum, err := SealStream(bw, key, plain, size)
	if err != nil {
		return err
	}
	if err := writeEnvTail(bw, raw, sum, sign); err != nil {
		return err
	}
	return bw.Flush()
}

// WriteEnvelopeCT 用已经加密好的密文(SealStream 的输出)拼一个信封:同一份副本发给几台备用时只加密一次,
// 每台只换头部(包给它的数据密钥)和签名。hdr.Size 由调用方填明文字节数。
func WriteEnvelopeCT(w io.Writer, hdr EnvHeader, ct io.Reader, sum [32]byte, sign func([]byte) []byte) error {
	hdr.V, hdr.CtLen = 1, ctLenFor(hdr.Size)
	raw, err := json.Marshal(hdr)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(w, 128<<10)
	if err := writeEnvHead(bw, raw); err != nil {
		return err
	}
	if _, err := io.CopyN(bw, ct, hdr.CtLen); err != nil {
		return fmt.Errorf("读密文: %w", err)
	}
	if err := writeEnvTail(bw, raw, sum, sign); err != nil {
		return err
	}
	return bw.Flush()
}

// EnvelopeSize 信封的总字节数(发送前填 Content-Length 用)。
func EnvelopeSize(hdr EnvHeader, sigLen int) (int64, error) {
	hdr.V, hdr.CtLen = 1, ctLenFor(hdr.Size)
	raw, err := json.Marshal(hdr)
	if err != nil {
		return 0, err
	}
	return int64(len(envMagic)+4+len(raw)) + hdr.CtLen + 32 + 2 + int64(sigLen), nil
}

func writeEnvHead(bw *bufio.Writer, raw []byte) error {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(raw)))
	if _, err := bw.WriteString(envMagic); err != nil {
		return err
	}
	if _, err := bw.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err := bw.Write(raw)
	return err
}

func writeEnvTail(bw *bufio.Writer, raw []byte, sum [32]byte, sign func([]byte) []byte) error {
	var sig []byte
	if sign != nil {
		sig = sign(signedBytes(raw, sum))
	}
	if _, err := bw.Write(sum[:]); err != nil {
		return err
	}
	var sl [2]byte
	binary.BigEndian.PutUint16(sl[:], uint16(len(sig)))
	if _, err := bw.Write(sl[:]); err != nil {
		return err
	}
	_, err := bw.Write(sig)
	return err
}

// SealStream 把 size 字节的明文按块加密写进 ct,返回密文的 sha256。
func SealStream(ct io.Writer, key []byte, plain io.Reader, size int64) ([32]byte, error) {
	var sum [32]byte
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return sum, err
	}
	h := sha256.New()
	buf := make([]byte, chunkSize)
	out := make([]byte, 0, chunkSize+chacha20poly1305.Overhead)
	var done int64
	for i := uint64(0); ; i++ {
		want := int64(chunkSize)
		if rest := size - done; rest < want {
			want = rest
		}
		if _, err := io.ReadFull(plain, buf[:want]); err != nil {
			return sum, fmt.Errorf("读明文: %w", err)
		}
		done += want
		last := done == size
		out = aead.Seal(out[:0], chunkNonce(i, last), buf[:want], nil)
		h.Write(out)
		if _, err := ct.Write(out); err != nil {
			return sum, err
		}
		if last {
			break
		}
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// signEd25519 用主机签名密钥签一段数据。
func signEd25519(k ed25519.PrivateKey, b []byte) []byte { return ed25519.Sign(k, b) }

func signedBytes(raw []byte, sum [32]byte) []byte {
	b := make([]byte, 0, len(sigContext)+len(raw)+32)
	b = append(b, sigContext...)
	b = append(b, raw...)
	return append(b, sum[:]...)
}

// SignedBytes 签名覆盖的内容。
func (e *Envelope) SignedBytes() []byte { return signedBytes(e.raw, e.CtSum) }

// VerifySig 用主机公钥验签。
func (e *Envelope) VerifySig(pub ed25519.PublicKey) error {
	if len(pub) != ed25519.PublicKeySize || len(e.Sig) != ed25519.SignatureSize || !ed25519.Verify(pub, e.SignedBytes(), e.Sig) {
		return errors.New("副本签名不对:不是当前主机签发的")
	}
	return nil
}

// ReceiveEnvelope 从 r 读一个完整信封并原样写进 w,同时校验结构、算出密文哈希;密文超过 maxCt 就停。
// 不解密:备用收到副本只验签、验哈希、落盘(解密留给自检和接任)。
func ReceiveEnvelope(r io.Reader, w io.Writer, maxCt int64) (*Envelope, error) {
	br := bufio.NewReaderSize(r, 128<<10)
	var head [len(envMagic) + 4]byte
	if _, err := io.ReadFull(br, head[:]); err != nil {
		return nil, fmt.Errorf("读信封头: %w", err)
	}
	if string(head[:len(envMagic)]) != envMagic {
		return nil, errors.New("不是 m-ui 容灾信封")
	}
	n := binary.BigEndian.Uint32(head[len(envMagic):])
	if n == 0 || n > maxHeader {
		return nil, errors.New("信封头部长度不对")
	}
	raw := make([]byte, n)
	if _, err := io.ReadFull(br, raw); err != nil {
		return nil, fmt.Errorf("读信封头: %w", err)
	}
	e := &Envelope{raw: raw, ctOff: int64(len(head)) + int64(n)}
	if err := json.Unmarshal(raw, &e.Header); err != nil {
		return nil, fmt.Errorf("信封头部不是 JSON: %w", err)
	}
	if e.Header.V != 1 || e.Header.CtLen != ctLenFor(e.Header.Size) {
		return nil, errors.New("信封版本或长度不对")
	}
	if maxCt > 0 && e.Header.CtLen > maxCt {
		return nil, fmt.Errorf("副本太大:%d 字节,上限 %d", e.Header.CtLen, maxCt)
	}
	if _, err := w.Write(head[:]); err != nil {
		return nil, err
	}
	if _, err := w.Write(raw); err != nil {
		return nil, err
	}
	h := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(w, h), br, e.Header.CtLen); err != nil {
		return nil, fmt.Errorf("读密文: %w", err)
	}
	var tail [34]byte
	if _, err := io.ReadFull(br, tail[:]); err != nil {
		return nil, fmt.Errorf("读信封尾: %w", err)
	}
	copy(e.CtSum[:], tail[:32])
	if !bytes.Equal(e.CtSum[:], h.Sum(nil)) {
		return nil, errors.New("密文哈希对不上:传输中损坏或被篡改")
	}
	sl := binary.BigEndian.Uint16(tail[32:])
	if sl > 512 {
		return nil, errors.New("签名长度不对")
	}
	e.Sig = make([]byte, sl)
	if _, err := io.ReadFull(br, e.Sig); err != nil {
		return nil, fmt.Errorf("读签名: %w", err)
	}
	if _, err := w.Write(tail[:]); err != nil {
		return nil, err
	}
	if _, err := w.Write(e.Sig); err != nil {
		return nil, err
	}
	if _, err := br.Peek(1); err != io.EOF {
		return nil, errors.New("信封后面还有多余的数据")
	}
	return e, nil
}

// OpenEnvelopeFile 读出一个落了盘的信封(结构与哈希都核对一遍)。
func OpenEnvelopeFile(path string) (*Envelope, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReceiveEnvelope(f, io.Discard, 0)
}

// DecryptFile 用数据密钥解密一个落了盘的信封,明文写进 dst。
func (e *Envelope) DecryptFile(path string, key []byte, dst io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(e.ctOff, io.SeekStart); err != nil {
		return err
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return err
	}
	br := bufio.NewReaderSize(io.LimitReader(f, e.Header.CtLen), 128<<10)
	buf := make([]byte, chunkSize+chacha20poly1305.Overhead)
	var out []byte
	rest := e.Header.CtLen
	for i := uint64(0); rest > 0; i++ {
		n := int64(len(buf))
		if rest < n {
			n = rest
		}
		if _, err := io.ReadFull(br, buf[:n]); err != nil {
			return fmt.Errorf("读密文: %w", err)
		}
		rest -= n
		out, err = aead.Open(out[:0], chunkNonce(i, rest == 0), buf[:n], nil)
		if err != nil {
			return errors.New("解密失败:密钥不对,或副本已损坏")
		}
		if _, err := dst.Write(out); err != nil {
			return err
		}
	}
	return nil
}

// ---- 数据密钥的包装 ----

// BoxKey 一对 X25519 密钥(备用的长期密钥,或设为主机时目标机器的一次性密钥)。
type BoxKey struct {
	Pub, Priv *[32]byte
}

// NewBoxKey 生成一对 X25519 密钥。
func NewBoxKey() (BoxKey, error) {
	pub, priv, err := box.GenerateKey(rand.Reader)
	return BoxKey{Pub: pub, Priv: priv}, err
}

// PubString 公钥的 base64。
func (k BoxKey) PubString() string { return base64.StdEncoding.EncodeToString(k.Pub[:]) }

// ParsePub 读 base64 的 32 字节公钥。
func ParsePub(s string) (*[32]byte, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != 32 {
		return nil, errors.New("公钥格式不对")
	}
	var k [32]byte
	copy(k[:], b)
	return &k, nil
}

// Fingerprint 公钥指纹:sha256 前 8 字节的十六进制,服务器页核对用。
func Fingerprint(pub string) string {
	if pub == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(pub))
	return hex.EncodeToString(sum[:8])
}

// SealTo 用收件人公钥匿名密封一段数据(数据密钥、通知块)。
func SealTo(data []byte, pub *[32]byte) (string, error) {
	out, err := box.SealAnonymous(nil, data, pub, rand.Reader)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(out), nil
}

// OpenWith 用自己的密钥打开 SealTo 密封的数据。
func OpenWith(sealed string, k BoxKey) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return nil, errors.New("密封数据格式不对")
	}
	out, ok := box.OpenAnonymous(nil, b, k.Pub, k.Priv)
	if !ok {
		return nil, errors.New("打不开:不是给本机的,或已损坏")
	}
	return out, nil
}

// FileSum 文件的 sha256(十六进制)。
func FileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---- 签名密钥 ----

// NewSignKey 生成主机签名密钥,返回存进本机设置的 base64 种子。
func NewSignKey() string {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(priv.Seed())
}

// SignKey 读本机设置里的签名密钥。
func SignKey(seed string) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(seed))
	if err != nil || len(b) != ed25519.SeedSize {
		return nil, errors.New("签名密钥格式不对")
	}
	return ed25519.NewKeyFromSeed(b), nil
}

// PubOf 签名公钥的 base64。
func PubOf(k ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(k.Public().(ed25519.PublicKey))
}

// ParseSignPub 读 base64 的签名公钥。
func ParseSignPub(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("主机签名公钥格式不对")
	}
	return ed25519.PublicKey(b), nil
}

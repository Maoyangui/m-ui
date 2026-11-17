// Package reward 流量奖励计划里面板这一侧不沾数据库的部分:面板身份(一把 Ed25519 密钥)、Solana 地址校验、
// 上报的签名、登记服务和公开账本的读取。数真实流量在 model.RewardCounter;状态和定时上报在 runner/reward.go;接口在 web/reward_api.go。
//
// 对外只有三种请求,而且各有前提:
//   - 读公开账本(奖池、逐笔入账、积分榜):GitHub 上本仓库 rewards 分支的 ledger.json(raw 和 jsDelivr 各取一次)。
//     没登记地址的面板只碰这一处,和查更新、读功德箱名单是同一类请求,不带任何面板自己的数据。
//   - 上报累计流量:登记了地址之后才发,内容只有 面板公钥、地址、累计流量(取整到 GB)、时间、签名。
//   - 留名捐赠:管理员在活动页点了「下一步」才发(留名、金额),之后查这一笔到没到账。
//
// 设置里关掉流量奖励后,三种都不发。
package reward

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// TB 一个积分对应的真实流量。
const TB = int64(1) << 40

// 设置里的键(都随主机走,见 ha/settings.go)。
const (
	KeyEnabled  = "rewardEnabled"  // 开关,默认开(只有明确设成 false 才算关)
	KeyAddress  = "rewardAddress"  // 登记的 Solana 地址
	KeyBoundAt  = "rewardBoundAt"  // 登记时间(unix 秒)
	KeyPriv     = "rewardKey"      // 面板身份:Ed25519 私钥(base64)。不发给浏览器、不进日志
	KeyRemind   = "rewardRemind"   // 提醒气泡上次被收起时的积分数;空 = 从没收起过(刚装好)
	KeyLast     = "rewardLast"     // 最近一次上报得到的答复(JSON,见 ReportReply)
	KeyDonation = "rewardDonation" // 正在等到账 / 刚到账的那笔留名捐赠(JSON,见 Intent)
)

// ---- Solana 地址 ----

const b58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// ValidSolana 是不是一个 Solana 地址:base58、解出来正好 32 字节。只验写法,不查链上有没有这个账户。
func ValidSolana(s string) bool {
	if len(s) < 32 || len(s) > 44 {
		return false
	}
	n := new(big.Int)
	radix := big.NewInt(58)
	for i := 0; i < len(s); i++ {
		d := strings.IndexByte(b58, s[i])
		if d < 0 {
			return false
		}
		n.Mul(n, radix)
		n.Add(n, big.NewInt(int64(d)))
	}
	size := len(n.Bytes())
	for i := 0; i < len(s) && s[i] == '1'; i++ { // 开头的每个 1 是一个 0 字节
		size++
	}
	return size == 32
}

// ---- 面板身份 ----

// NewKey 生成一把新的面板密钥,返回存进设置里的写法。
func NewKey() (string, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(priv), nil
}

// ParseKey 把设置里存的密钥读回来。
func ParseKey(s string) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, errors.New("面板的奖励密钥读不出来")
	}
	return ed25519.PrivateKey(b), nil
}

// PubID 面板在登记处的身份:公钥的 base64url(不带补位)。
func PubID(priv ed25519.PrivateKey) string {
	return base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
}

// Canonical 上报里被签名的那段文字。登记服务(deploy/reward)按同样的写法拼出来验。
func Canonical(pub, address string, gb, ts int64) string {
	return "m-ui-reward-v1\n" + pub + "\n" + address + "\n" + strconv.FormatInt(gb, 10) + "\n" + strconv.FormatInt(ts, 10)
}

// Report 一次上报。只有这些:不带域名、IP、用户数、线路或任何用户数据。
type Report struct {
	V       int    `json:"v"`
	Pub     string `json:"pub"`
	Address string `json:"address"`
	GB      int64  `json:"gb"` // 累计真实流量,向下取整到 GB
	TS      int64  `json:"ts"`
	Sig     string `json:"sig"`
}

// NewReport 按现在的累计值签一份上报。
func NewReport(priv ed25519.PrivateKey, address string, bytesTotal, now int64) Report {
	r := Report{V: 1, Pub: PubID(priv), Address: address, GB: bytesTotal >> 30, TS: now}
	r.Sig = base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(Canonical(r.Pub, r.Address, r.GB, r.TS))))
	return r
}

// Verify 验一份上报的签名(登记服务那边是 JS 写的同一套;这里给测试和自检用)。
func (r Report) Verify() bool {
	pub, err := base64.RawURLEncoding.DecodeString(r.Pub)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(r.Sig)
	if err != nil {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), []byte(Canonical(r.Pub, r.Address, r.GB, r.TS)), sig)
}

// Excluded 这台面板被系统剔除了:它的流量现在不计入积分。判的是面板不是地址 —— 别人把你的地址绑到他作弊的面板上,影响不到你。
// Rule 是规则的代号(页面按它用界面的语言说明原因);Reason 是登记服务写的那句话(不认识的代号就原样显示它)。
type Excluded struct {
	Rule   string `json:"rule,omitempty"`
	Reason string `json:"reason"`
	At     int64  `json:"at"`
}

// ReportReply 登记服务对一次上报的答复。
type ReportReply struct {
	GB       int64     `json:"gb"`                 // 登记处现在记着的累计值(GB);比本机大 = 本机是接任 / 还原回来的,按它往上接
	Credited int64     `json:"credited"`           // 这个地址已经入账的积分(每个地址每天最多入账 1 分)
	Excluded *Excluded `json:"excluded,omitempty"` // 这台面板现在被剔除着
	At       int64     `json:"at"`                 // 答复的时间(本机记的)
}

// ---- 公开账本 ----

// Chain 一条链上的收款地址,和计入奖池的那些入账的合计(订单按全额记)。
type Chain struct {
	Chain    string  `json:"chain"` // solana | tron
	Address  string  `json:"address"`
	Received float64 `json:"received"`
}

// Deposit 一笔入账。
type Deposit struct {
	TS     int64   `json:"ts"`
	Kind   string  `json:"kind"` // order | donation
	Chain  string  `json:"chain"`
	Amount float64 `json:"amount"`
	From   string  `json:"from"`
	Tx     string  `json:"tx"`             // 链上交易号;人工确认发放、又没对上链上那一笔的订单没有(空)
	Name   string  `json:"name,omitempty"` // 捐赠的留名。只有留名捐赠计入奖池,所以捐赠一定带名字
	Note   string  `json:"note,omitempty"` // manual = 人工确认发放的订单(付款金额和订单对不上,订阅站核对之后发放的;金额是实际到账的)
	Void   bool    `json:"void,omitempty"` // 作废:监听到账之后订单又取消(退款)了。留在账本里给人看,不计入奖池
}

// BoardRow 积分榜上的一个地址。Excluded = 它名下的面板现在全都被剔除着(原因见 Rule / Reason)。
type BoardRow struct {
	Address  string `json:"address"`
	Points   int64  `json:"points"`
	Panels   int    `json:"panels"`
	Excluded bool   `json:"excluded,omitempty"`
	Rule     string `json:"rule,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// PoolSum 登记服务算好的合计(面板不自己加,免得两边取整不一样)。
type PoolSum struct {
	Total        float64 `json:"total"`
	OrderCount   int     `json:"orderCount"`
	OrderAmount  float64 `json:"orderAmount"`
	OrderShare   float64 `json:"orderShare"`
	DonateCount  int     `json:"donateCount"`
	DonateAmount float64 `json:"donateAmount"`
	Points       int64   `json:"points"`
	Addresses    int     `json:"addresses"`
}

// DonateInfo 往哪儿捐。
type DonateInfo struct {
	Chain   string  `json:"chain"`
	Address string  `json:"address"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
}

// Ledger 公开账本(rewards 分支的 ledger.json;登记服务的 /export 是同一份)。
type Ledger struct {
	V         int        `json:"v"`
	UpdatedAt int64      `json:"updatedAt"`
	PayoutAt  int64      `json:"payoutAt"`
	OrderRate float64    `json:"orderRate"`
	Pool      PoolSum    `json:"pool"`
	Chains    []Chain    `json:"chains"`
	Donate    DonateInfo `json:"donate"`
	Deposits  []Deposit  `json:"deposits"`
	Board     []BoardRow `json:"board"`
}

// Intent 一笔留名捐赠:登记服务给出要转的确切金额和期限,期限内这个数到账就以这个名字记上。
type Intent struct {
	Id        string  `json:"id"`
	Name      string  `json:"name"`
	Amount    float64 `json:"amount"` // 填的数
	Exact     float64 `json:"exact"`  // 要转的数(和别人撞了会差几分钱)
	Chain     string  `json:"chain"`
	Address   string  `json:"address"`
	CreatedAt int64   `json:"createdAt"`
	ExpiresAt int64   `json:"expiresAt"`
	Status    string  `json:"status"` // pending | paid | expired
}

// ---- 请求 ----

const (
	maxLedgerBytes = 8 << 20
	maxReplyBytes  = 64 << 10
)

// Client 对外的三种请求。API 是登记服务的地址;LedgerURLs 是公开账本的地址(依次试,先拿到合法的那份就用)。
type Client struct {
	API        string
	LedgerURLs []string
	HTTP       *http.Client
	UserAgent  string
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// apiError 登记服务给的错(原样给管理员看)。
type apiError struct {
	Status int
	Msg    string
}

func (e apiError) Error() string { return e.Msg }

// StatusOf 这个错是不是登记服务明确答复的,是的话给出它的状态码(不是则为 0:网络不通、答复看不懂)。
func StatusOf(err error) int {
	var e apiError
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

func (c *Client) call(ctx context.Context, method, path string, in, out interface{}) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.API, "/")+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("连不上登记服务: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes))
	if err != nil {
		return fmt.Errorf("读登记服务的答复: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
			Code  string `json:"code"`
			Days  int64  `json:"days"`
		}
		_ = json.Unmarshal(raw, &e)
		if known := registryError(e.Code, e.Days); known != nil {
			e.Error = known.Error()
		}
		if e.Error == "" {
			e.Error = fmt.Sprintf("登记服务答复 %d", resp.StatusCode)
		}
		return apiError{Status: resp.StatusCode, Msg: e.Error}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("登记服务的答复看不懂: %w", err)
	}
	return nil
}

// registryError 登记服务的错带一个代号。认识的代号用面板自己的说法(英文界面靠它才有译文);
// 不认识的(登记服务比面板新)返回 nil,原样显示登记服务写的那句话。
func registryError(code string, days int64) error {
	switch code {
	case "address":
		return errors.New("登记服务不认这个地址:它不是 Solana 地址")
	case "clock":
		return errors.New("这台服务器的时间和登记服务差得太多,请先校准服务器时间")
	case "format", "sig":
		return errors.New("登记服务不认这次请求,请把面板升到最新版再试")
	case "busy_src":
		return errors.New("这台服务器所在的来源今天登记的面板太多了,明天再试")
	case "cooldown":
		if days < 1 {
			days = 1
		}
		return fmt.Errorf("换领取地址有 7 天的冷却期,还要等 %d 天", days)
	case "name_empty":
		return errors.New("请填写留名")
	case "name_chars":
		return errors.New("留名里有不能用的字符")
	case "name_long":
		return errors.New("留名最多 20 个字")
	case "name_url":
		return errors.New("留名不能是网址")
	case "amount":
		return errors.New("金额要在 1 到 100000 之间,最多两位小数")
	case "busy":
		return errors.New("提交得太勤了,过一会儿再试")
	case "no_pool":
		return errors.New("现在没有可用的奖池地址,请稍后再试")
	case "no_slot":
		return errors.New("这个金额现在排不开,换个数再试")
	case "not_found":
		return errors.New("登记服务那边已经没有这一笔留名了")
	case "internal":
		return errors.New("登记服务出错了,请稍后再试")
	}
	return nil
}

// Report 上报一次。
func (c *Client) Report(ctx context.Context, r Report) (ReportReply, error) {
	var out ReportReply
	err := c.call(ctx, http.MethodPost, "/report", r, &out)
	return out, err
}

// Donate 登记一笔留名捐赠,拿回要转的确切金额和期限。
func (c *Client) Donate(ctx context.Context, name string, amount float64) (Intent, error) {
	var out Intent
	err := c.call(ctx, http.MethodPost, "/donate", map[string]interface{}{"name": name, "amount": amount}, &out)
	return out, err
}

// DonateStatus 这一笔到没到账。
func (c *Client) DonateStatus(ctx context.Context, id string) (Intent, error) {
	var out Intent
	err := c.call(ctx, http.MethodGet, "/donate/"+id, nil, &out)
	return out, err
}

// DonateCancel 不捐了(只是把占着的金额让出来;已经转出去的钱照样进奖池,记为匿名)。
func (c *Client) DonateCancel(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodPost, "/donate/"+id+"/cancel", nil, nil)
}

// Export 直接向登记服务要一份账本(登记了地址的面板用它,比分支上的新)。
func (c *Client) Export(ctx context.Context) (*Ledger, error) {
	return c.fetchLedger(ctx, strings.TrimRight(c.API, "/")+"/export")
}

// FetchLedger 读公开账本:几个地址依次试。
func (c *Client) FetchLedger(ctx context.Context) (*Ledger, error) {
	var last error
	for _, u := range c.LedgerURLs {
		l, err := c.fetchLedger(ctx, u)
		if err == nil {
			return l, nil
		}
		last = err
	}
	if last == nil {
		last = errors.New("没有配置公开账本的地址")
	}
	return nil, last
}

func (c *Client) fetchLedger(ctx context.Context, u string) (*Ledger, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("账本地址答复 %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxLedgerBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxLedgerBytes {
		return nil, errors.New("账本太大")
	}
	var l Ledger
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, fmt.Errorf("账本看不懂: %w", err)
	}
	if err := l.check(time.Now().Unix()); err != nil {
		return nil, err
	}
	return &l, nil
}

// check 外面取来的账本先过一遍:版本、时间、比例都要说得通,地址得像地址。面板上原样显示这些内容(文字都走 textContent),
// 这里是第二道:写坏的、被换掉的文件不拿来用。
func (l *Ledger) check(now int64) error {
	if l.V != 1 {
		return fmt.Errorf("不认识的账本版本 %d", l.V)
	}
	if l.UpdatedAt <= 0 || l.UpdatedAt > now+600 {
		return errors.New("账本的更新时间不对")
	}
	if l.OrderRate <= 0 || l.OrderRate > 1 {
		return errors.New("账本里订单计入的比例不对")
	}
	if l.Pool.Total < 0 || l.Pool.Points < 0 {
		return errors.New("账本里的合计不对")
	}
	for i := range l.Chains {
		c := &l.Chains[i]
		if c.Chain != "solana" && c.Chain != "tron" {
			return fmt.Errorf("账本里有不认识的链 %q", c.Chain)
		}
		if !validChainAddr(c.Chain, c.Address) {
			return fmt.Errorf("账本里 %s 的收款地址不对", c.Chain)
		}
	}
	if l.Donate.Address != "" && !validChainAddr(l.Donate.Chain, l.Donate.Address) {
		return errors.New("账本里的捐赠地址不对")
	}
	for i := range l.Deposits {
		d := &l.Deposits[i]
		if d.Kind != "order" && d.Kind != "donation" {
			return fmt.Errorf("账本第 %d 笔入账的类型不对", i+1)
		}
		if d.Kind == "donation" && d.Name == "" {
			return fmt.Errorf("账本第 %d 笔入账是捐赠却没有留名", i+1)
		}
		if d.Amount < 0 || len(d.From) > 64 || len(d.Tx) > 128 || len([]rune(d.Name)) > 24 {
			return fmt.Errorf("账本第 %d 笔入账写得不对", i+1)
		}
		if (d.Note != "" && (d.Note != "manual" || d.Kind != "order")) || (d.Void && d.Kind != "order") {
			return fmt.Errorf("账本第 %d 笔入账的标记不对", i+1)
		}
		if d.Tx == "" && d.Note != "manual" { // 只有人工确认的订单可以没有交易号
			return fmt.Errorf("账本第 %d 笔入账没有交易号", i+1)
		}
	}
	for i := range l.Board {
		b := &l.Board[i]
		if !ValidSolana(b.Address) || b.Points < 0 || len([]rune(b.Reason)) > 200 || len(b.Rule) > 32 {
			return fmt.Errorf("账本积分榜第 %d 行写得不对", i+1)
		}
	}
	return nil
}

func validChainAddr(chain, a string) bool {
	if chain == "tron" {
		if len(a) != 34 || a[0] != 'T' {
			return false
		}
		for i := 0; i < len(a); i++ {
			if strings.IndexByte(b58, a[i]) < 0 {
				return false
			}
		}
		return true
	}
	return ValidSolana(a)
}

// Share 这笔入账计入奖池多少:订单按比例,捐赠全额;作废的不计入。
func (l *Ledger) Share(d Deposit) float64 {
	if d.Void {
		return 0
	}
	if d.Kind == "donation" {
		return d.Amount
	}
	return round2(d.Amount * l.OrderRate)
}

func round2(v float64) float64 {
	if v < 0 {
		return -round2(-v)
	}
	return float64(int64(v*100+0.5)) / 100
}

// CleanName 留名怎么收:去掉首尾空白、把连着的空白并成一个;不许有控制字符、换行,也不许是网址;最多 20 个字。
func CleanName(s string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "", errors.New("请填写留名")
	}
	n := 0
	for _, r := range s {
		n++
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x200b || r == 0x202e || r == 0xfeff {
			return "", errors.New("留名里有不能用的字符")
		}
	}
	if n > 20 {
		return "", errors.New("留名最多 20 个字")
	}
	low := strings.ToLower(s)
	if strings.Contains(low, "://") || strings.Contains(low, "www.") {
		return "", errors.New("留名不能是网址")
	}
	return s, nil
}

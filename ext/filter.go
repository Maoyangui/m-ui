package ext

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Maoyangui/m-ui/database/model"
)

// Filter 外部节点输出到用户订阅前的筛选与改名。
//
// 节点的身份是"名字 + 指纹"(Node.Key,由地址、端口、凭据、SNI、Host、路径算出,不含名字):
//   - 按指纹记的(HiddenKeys / RenameKeys)不怕刷新后改名、换顺序:信息节点"剩余流量 120.53 GB"下次变成"118.07 GB",
//     机场删掉一个节点后"香港 04"改叫"香港 03",都还认得;
//   - 按名字记的(Hidden / Renames)是老数据(只按名字),以及名字唯一的节点顺带记下的名字:机场重置订阅换了密码、
//     指纹全变时,还能按名字认出来;
//   - 两者命中其一就算。按名字的记录如果和某条按指纹的记录是同一次操作记下的(配对),而那个指纹的节点还在列表里、
//     只是换了名字,这条名字记录就不再套到别的节点头上 —— 否则机场重新编号后,"香港 04"这个名字会扣到另一个节点上;
//   - 同名的节点(或同一指纹有好几个节点)只按指纹记,名字记了会连带别的。
type Filter struct {
	Hidden     map[string]bool      // 按名字隐藏
	Renames    map[string]string    // 按名字改名:名字 → 新名
	HiddenKeys map[string]string    // 按指纹隐藏:指纹 → 隐藏时的名字(配对用;空 = 只按指纹认)
	RenameKeys map[string]KeyRename // 按指纹改名
	Exclude    []string             // 名字含这些关键词的不输出(不分大小写):官网、到期、剩余流量这类信息节点
}

// KeyRename 一条按指纹的改名。
type KeyRename struct {
	From string `json:"from,omitempty"` // 改名时节点的原名(配对用;空 = 只按指纹认)
	To   string `json:"to"`
}

// MaxPrefEntries 每类隐藏 / 改名记录的上限。改成合并保存后,列表里暂时没有的节点的记录不再被顺手抹掉,
// 会一直攒着,要有个头;超了让管理员先清理未匹配的记录。
const MaxPrefEntries = 2000

// maxNameBytes 按名字记的名字、改成的新名的长度上限(超长的节点名只按指纹记)。
const maxNameBytes = 512

// maxOpNameBytes 面板传来的节点名的长度上限(防一次塞进超长串)。
const maxOpNameBytes = 4096

// ParseFilter 从 ExtNode 的筛选字段组出筛选条件,解析不了的当没有。
func ParseFilter(hidden, renames, hiddenKeys, renameKeys json.RawMessage, exclude string) Filter {
	f := Filter{Hidden: map[string]bool{}, Renames: map[string]string{}, HiddenKeys: map[string]string{}, RenameKeys: map[string]KeyRename{}}
	var list []string
	if len(hidden) > 0 && json.Unmarshal(hidden, &list) == nil {
		for _, n := range list {
			if strings.TrimSpace(n) != "" {
				f.Hidden[n] = true
			}
		}
	}
	if len(renames) > 0 {
		var m map[string]string
		if json.Unmarshal(renames, &m) == nil {
			for from, to := range m {
				if strings.TrimSpace(to) != "" {
					f.Renames[from] = to
				}
			}
		}
	}
	if len(hiddenKeys) > 0 {
		var m map[string]string
		if json.Unmarshal(hiddenKeys, &m) == nil {
			for k, n := range m {
				if k != "" {
					f.HiddenKeys[k] = n
				}
			}
		}
	}
	if len(renameKeys) > 0 {
		var m map[string]KeyRename
		if json.Unmarshal(renameKeys, &m) == nil {
			for k, r := range m {
				if k != "" && strings.TrimSpace(r.To) != "" {
					f.RenameKeys[k] = r
				}
			}
		}
	}
	for _, k := range strings.FieldsFunc(exclude, func(r rune) bool {
		return r == ',' || r == '，' || r == '|' || r == '、' || r == '\n' || r == ';' || r == '；'
	}) {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			f.Exclude = append(f.Exclude, k)
		}
	}
	return f
}

// FilterOf 一条外部节点的筛选条件。
func FilterOf(e model.ExtNode) Filter {
	return ParseFilter(e.Hidden, e.Renames, e.HiddenKeys, e.RenameKeys, e.Exclude)
}

// Encode 写回库里的四个字段(名字排好序,内容一样编码就一样)。
func (f Filter) Encode() (hidden, renames, hiddenKeys, renameKeys json.RawMessage) {
	names := make([]string, 0, len(f.Hidden))
	for n := range f.Hidden {
		names = append(names, n)
	}
	sort.Strings(names)
	hidden, _ = json.Marshal(names)
	renames, _ = json.Marshal(nonNil(f.Renames))
	hiddenKeys, _ = json.Marshal(nonNil(f.HiddenKeys))
	if f.RenameKeys == nil {
		f.RenameKeys = map[string]KeyRename{}
	}
	renameKeys, _ = json.Marshal(f.RenameKeys)
	return
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// Empty 没有任何筛选与改名。
func (f Filter) Empty() bool {
	return len(f.Hidden) == 0 && len(f.Renames) == 0 && len(f.HiddenKeys) == 0 && len(f.RenameKeys) == 0 && len(f.Exclude) == 0
}

// UsesKeys 用到了按指纹的记录(0.7.17 之前的副机不认识)。
func (f Filter) UsesKeys() bool { return len(f.HiddenKeys) > 0 || len(f.RenameKeys) > 0 }

// UsesNames 用到了按名字的隐藏、改名或排除关键词(0.7.16 之前的副机不认识)。
func (f Filter) UsesNames() bool {
	return len(f.Hidden) > 0 || len(f.Renames) > 0 || len(f.Exclude) > 0
}

// Decision 一个节点的筛选结果。
type Decision struct {
	Hidden  bool   // 不输出
	Keyword bool   // 是关键词排除的(面板上开关不可点:手动显示不了,要改关键词)
	Rename  string // 改成的名字;空 = 不改
}

// Roster 一份节点列表的名字与指纹(判定"名字唯一""指纹还在不在"要看整份列表)。
type Roster struct {
	Names []string
	Keys  []string
}

// RosterOf 节点列表的名字与指纹。
func RosterOf(it Items) Roster {
	r := Roster{Names: make([]string, len(it.Nodes)), Keys: make([]string, len(it.Nodes))}
	for i, n := range it.Nodes {
		r.Names[i], r.Keys[i] = n.Name, n.Key
	}
	return r
}

// unique 这个名字在列表里只有一个节点、它的指纹就是 key、这个指纹也只有它一个:只有这样按名字记才不会连带别的节点。
func (r Roster) unique(name, key string) bool {
	if strings.TrimSpace(name) == "" || len(name) > maxNameBytes {
		return false
	}
	byName, byKey := 0, 0
	for i := range r.Names {
		if r.Names[i] == name {
			byName++
			if r.Keys[i] != key {
				return false
			}
		}
		if r.Keys[i] == key {
			byKey++
		}
	}
	return byName == 1 && byKey == 1
}

func (r Roster) hasKey(key string) bool {
	for _, k := range r.Keys {
		if k == key {
			return true
		}
	}
	return false
}

func (r Roster) hasName(name string) bool {
	for _, n := range r.Names {
		if n == name {
			return true
		}
	}
	return false
}

// Decide 对整份列表逐个判定。
func (f Filter) Decide(r Roster) []Decision {
	present := make(map[string]bool, len(r.Keys))
	for _, k := range r.Keys {
		present[k] = true
	}
	// 被认领的名字记录:和它配对的指纹还在列表里(那个节点只是换了名字),这个名字就不再按名字套到别的节点头上
	claimedHide := map[string]bool{}
	for k, n := range f.HiddenKeys {
		if n != "" && present[k] {
			claimedHide[n] = true
		}
	}
	claimedRename := map[string]bool{}
	for k, rk := range f.RenameKeys {
		if rk.From != "" && present[k] {
			claimedRename[rk.From] = true
		}
	}
	out := make([]Decision, len(r.Names))
	for i, name := range r.Names {
		key := r.Keys[i]
		d := &out[i]
		if f.keyword(name) {
			d.Hidden, d.Keyword = true, true
		} else if _, ok := f.HiddenKeys[key]; ok {
			d.Hidden = true
		} else if f.Hidden[name] && !claimedHide[name] {
			d.Hidden = true
		}
		if rk, ok := f.RenameKeys[key]; ok {
			d.Rename = rk.To
		} else if to := f.Renames[name]; to != "" && !claimedRename[name] {
			d.Rename = to
		}
	}
	return out
}

func (f Filter) keyword(name string) bool {
	low := strings.ToLower(name)
	for _, k := range f.Exclude {
		if strings.Contains(low, k) {
			return true
		}
	}
	return false
}

// Apply 按筛选条件处理节点:隐藏的拿掉、改名的换名字。链接和 Clash 用同一个名字、同一个指纹判定。
func (f Filter) Apply(it Items) Items {
	if f.Empty() {
		return it
	}
	dec := f.Decide(RosterOf(it))
	out := Items{Nodes: make([]Node, 0, len(it.Nodes))}
	for i, n := range it.Nodes {
		if dec[i].Hidden {
			continue
		}
		if to := dec[i].Rename; to != "" && to != n.Name {
			n = n.renamed(to)
		}
		out.Nodes = append(out.Nodes, n)
	}
	return out
}

// ---- 面板上的增量修改:隐藏 / 显示 / 改名,在服务端按当前完整列表合并进已有记录 ----

// Hide 隐藏一个节点。
func (f *Filter) Hide(r Roster, name, key string) {
	if r.unique(name, key) {
		if old := f.HiddenKeys[key]; old != "" && old != name && !r.hasName(old) {
			delete(f.Hidden, old) // 这个节点以前的名字(机场改过名):换成现在的名字配对,旧名字留着只会成为对不上的记录
		}
		f.Hidden[name] = true
		f.HiddenKeys[key] = name
		return
	}
	if _, ok := f.HiddenKeys[key]; !ok {
		f.HiddenKeys[key] = ""
	}
}

// Show 显示一个节点。它要是靠名字隐藏的、同名的还有别的节点也靠这条名字隐藏着,先把那些拆成按指纹的记录,只放出这一个。
func (f *Filter) Show(r Roster, name, key string) {
	if pair, ok := f.HiddenKeys[key]; ok {
		delete(f.HiddenKeys, key)
		if pair != "" && pair != name {
			delete(f.Hidden, pair) // 同一次隐藏记下的旧名字:节点已经换了名,留着只会误伤以后用这个名字的节点
		}
	}
	if !f.Hidden[name] {
		return
	}
	dec := f.Decide(r)
	for i := range r.Names {
		if r.Names[i] == name && r.Keys[i] != key && dec[i].Hidden && !dec[i].Keyword {
			if _, ok := f.HiddenKeys[r.Keys[i]]; !ok {
				f.HiddenKeys[r.Keys[i]] = ""
			}
		}
	}
	delete(f.Hidden, name)
}

// Rename 改名;to 为空或等于原名就是恢复原名。
func (f *Filter) Rename(r Roster, name, key, to string) {
	to = strings.TrimSpace(to)
	if rk, ok := f.RenameKeys[key]; ok {
		delete(f.RenameKeys, key)
		if rk.From != "" && rk.From != name && f.Renames[rk.From] == rk.To {
			delete(f.Renames, rk.From)
		}
	}
	if old := f.Renames[name]; old != "" && (to == "" || to == name || !r.unique(name, key)) {
		// 这条按名字的改名同名的别的节点也在用:拆成按指纹的记录,别跟着这个节点一起变
		dec := f.Decide(r)
		for i := range r.Names {
			if r.Names[i] == name && r.Keys[i] != key && dec[i].Rename == old {
				if _, ok := f.RenameKeys[r.Keys[i]]; !ok {
					f.RenameKeys[r.Keys[i]] = KeyRename{To: old}
				}
			}
		}
		delete(f.Renames, name)
	}
	if to == "" || to == name {
		return
	}
	if r.unique(name, key) {
		f.Renames[name] = to
		f.RenameKeys[key] = KeyRename{From: name, To: to}
		return
	}
	f.RenameKeys[key] = KeyRename{To: to}
}

// Stale 当前列表里一个节点都对不上的记录条数(节点临时下线、改了名、换了密码都会这样)。
func (f Filter) Stale(r Roster) int {
	n := 0
	for name := range f.Hidden {
		if !r.hasName(name) {
			n++
		}
	}
	for name := range f.Renames {
		if !r.hasName(name) {
			n++
		}
	}
	for k := range f.HiddenKeys {
		if !r.hasKey(k) {
			n++
		}
	}
	for k := range f.RenameKeys {
		if !r.hasKey(k) {
			n++
		}
	}
	return n
}

// DropStale 删掉对不上的记录(管理员在面板上点"清理"才做:列表里暂时没有的节点回来时会重新输出)。
func (f *Filter) DropStale(r Roster) {
	for name := range f.Hidden {
		if !r.hasName(name) {
			delete(f.Hidden, name)
		}
	}
	for name := range f.Renames {
		if !r.hasName(name) {
			delete(f.Renames, name)
		}
	}
	for k := range f.HiddenKeys {
		if !r.hasKey(k) {
			delete(f.HiddenKeys, k)
		}
	}
	for k := range f.RenameKeys {
		if !r.hasKey(k) {
			delete(f.RenameKeys, k)
		}
	}
}

// ErrTooManyPrefs 记录超过上限。
var ErrTooManyPrefs = fmt.Errorf("隐藏 / 改名记录超过 %d 条,请先清理未匹配的记录", MaxPrefEntries)

// Check 记录条数在限度内。
func (f Filter) Check() error {
	if len(f.Hidden) > MaxPrefEntries || len(f.Renames) > MaxPrefEntries || len(f.HiddenKeys) > MaxPrefEntries || len(f.RenameKeys) > MaxPrefEntries {
		return ErrTooManyPrefs
	}
	return nil
}

// ValidName 面板传来的节点名是不是合法文本(UTF-8、不超长)。
func ValidName(s string) bool { return len(s) <= maxOpNameBytes && utf8.ValidString(s) }

// ValidRename 改成的新名是不是合法文本。
func ValidRename(s string) bool { return len(s) <= maxNameBytes && utf8.ValidString(s) }

// LinkName 分享链接里的节点名,和面板列表、Clash 订阅认的是同一个(见 linkNode)。
func LinkName(link string) string { return linkNode(link).Name }

// RenameLink 给分享链接换名字:vmess 改 base64 里的 ps,其余换掉第一个 # 之后的部分。
func RenameLink(link, name string) string {
	base, _, _ := strings.Cut(link, "#")
	if len(base) >= len("vmess://") && strings.EqualFold(base[:len("vmess://")], "vmess://") {
		if b, err := decodeBase64Loose(base[len("vmess://"):]); err == nil {
			var v map[string]interface{}
			if json.Unmarshal([]byte(b), &v) == nil {
				v["ps"] = name
				if enc, err := json.Marshal(v); err == nil {
					return "vmess://" + base64.StdEncoding.EncodeToString(enc)
				}
			}
		}
	}
	return base + "#" + url.PathEscape(name)
}

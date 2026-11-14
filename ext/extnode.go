package ext

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/Maoyangui/m-ui/database/model"
)

// ItemsOf 一条外部节点的节点列表:分享链接直接解析,外部订阅解析主机抓到的缓存。
func ItemsOf(e model.ExtNode) Items {
	if e.Type == "link" {
		return Parse(e.Value)
	}
	return Parse(e.Cache)
}

// Output 输出到用户订阅的节点:筛选、改名、加前缀之后。订阅服务(主机与副机)和修订号都按这一份。
func Output(e model.ExtNode) Items {
	return WithPrefix(FilterOf(e).Apply(ItemsOf(e)), e.Prefix)
}

// Counts 输出的节点数与解析出的总数。
func Counts(e model.ExtNode) (out, total int) {
	it := ItemsOf(e)
	return len(FilterOf(e).Apply(it).Nodes), len(it.Nodes)
}

// outputSigMemo OutputSig 的结果按输入内容缓存:主机每 5 秒为每条外部节点算一次,内容多半没变,不必每次都解析一遍。
var outputSigMemo = struct {
	sync.Mutex
	m map[[32]byte]string
}{m: map[[32]byte]string{}}

// OutputSig 输出到用户订阅的节点集合的指纹(主副同步的修订号用它代替整份缓存):
// 被隐藏的信息节点数字变了、抓到的原文换了排版,输出没变就不算配置变了,不再每次刷新都向所有副机推一遍全量快照。
func OutputSig(e model.ExtNode) string {
	h := sha256.New()
	for _, s := range []string{e.Type, e.Value, e.Cache, string(e.Hidden), string(e.Renames), string(e.HiddenKeys), string(e.RenameKeys), e.Exclude, e.Prefix} {
		fmt.Fprintf(h, "%d:%s\x00", len(s), s)
	}
	var in [32]byte
	copy(in[:], h.Sum(nil))
	outputSigMemo.Lock()
	sig, ok := outputSigMemo.m[in]
	outputSigMemo.Unlock()
	if ok {
		return sig
	}
	out := sha256.New()
	for _, n := range Output(e).Nodes {
		c, _ := json.Marshal(n.Clash)
		fmt.Fprintf(out, "%s\x00%s\x00%s\n", n.Name, n.Link, c)
	}
	sig = hex.EncodeToString(out.Sum(nil))[:16]
	outputSigMemo.Lock()
	if len(outputSigMemo.m) > 1024 { // 外部节点不会多到这个数;防的是内容不断变化时无限增长
		outputSigMemo.m = map[[32]byte]string{}
	}
	outputSigMemo.m[in] = sig
	outputSigMemo.Unlock()
	return sig
}

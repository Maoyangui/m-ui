package camo

// 图包(用户 11-19:「伪装站的封面都不行」):视频站、个人博客、软件下载页的封面用照片。
//
// 82 张照片放在仓库的 camo-media 分支,不进程序本体。服务器开着这几种伪装站时,才把这一站用到的那几张取下来
// (runner 里做:先 raw.githubusercontent.com,取不到走 jsDelivr),按 MediaFiles 核对 sha256,再按本机种子处理(Process):
// 放大取景、可能左右翻转、微调亮度 / 对比度 / 饱和度 / 色温、重新压缩 —— 同一张照片在每台服务器上构图、色调、字节都不一样,
// 公开的原图和伪装站上的图对不上字节。取不到(或还没取到)时照旧用画出来的插图,页面上的文字不变。
//
// 文件名是「类别-天色-两位序号.jpg」:类别就是封面的场景(cover 的 kind),天色是 dawn / day / dusk / night / rain,
// 和天色无关的物件照(录音棚、机柜、胶片……)写 any。

import (
	"hash/fnv"
	"sort"
	"strings"
)

// MediaBranch 图包所在的分支
const MediaBranch = "camo-media"

// MediaMaxBytes 单张原图的上限(取图时超过就不要)
const MediaMaxBytes = 512 << 10

// MediaFiles 图包:文件名 → sha256(小写十六进制)。换图、加图都要改这里(旧版本程序只认它自己清单里的)。
var MediaFiles = map[string]string{
	"bike-any-01.jpg":      "ee08d2b901a22e63ec06ecf20cdab7329fdab23f30f0e885e086d612e005708d",
	"bike-any-02.jpg":      "43d5fa31631ca96515121db1972dd9a1fcb783a89e08174a30cfaf15eb3d1f05",
	"bike-any-03.jpg":      "718e208e80efe9832a7c99049e9fbd47781291960ba3b2724848a28867c21d94",
	"city-day-01.jpg":      "62d2d74e2185158f4afda562f07f98533b38d2d9886932d1cad6466b39331dfb",
	"city-dusk-01.jpg":     "a8676f0354bb0349a41239255a57eee2b37184a5f618aa4d15cfb4ef474afe21",
	"city-dusk-02.jpg":     "f405d1bbc9ff79818d7e071d5c6efdaa8cc65503a4225949a931afdc96ada32e",
	"city-night-01.jpg":    "ad1c637ff03a158aae14b39002ec055f55b5b258d5523b3600dd3790e4a79912",
	"city-night-02.jpg":    "b18e672725ba766eee3f73eefbf2eb81ebddf3cef51d9a070922012426f5b36f",
	"city-night-03.jpg":    "1e6bc0fd82d747b7854e5c0a3b1dfda7e1f6b891a7c8d181e959eda0bbbca5bc",
	"clouds-dawn-01.jpg":   "dc35da99cb10c065a29ffd5b074dc4692f9d96070e1297bb72c4fc83de94a704",
	"clouds-dawn-02.jpg":   "81ee47cb859117c073b44bacbd70f253a81f348021f53c7571e061d2511ff935",
	"clouds-dawn-03.jpg":   "5ac657478e0220d582d553f60b663228c14920568d84c7d6df4a29b5b23e458a",
	"desert-day-01.jpg":    "09f44413197a7ec798452ff3a6c189e581a4c96e952e7b3ce134fb8b86e75cdf",
	"desert-day-02.jpg":    "b8049f7e9902874fcc357de92ba836db8e626ec025e609a72c8512c575c5bf42",
	"desert-dusk-01.jpg":   "de877564de86d37726a426725dfa1c1bb2b0155e7b6ca64bb209a9146a2a6125",
	"desert-dusk-02.jpg":   "9fb1f46be18f71088c0dd44c31c4a0a1447ddefab8631e20dd32823815717287",
	"disc-any-01.jpg":      "bddd9e23fff9828e12c64bfbc490e389b03b8d968c3506f68b6bd29d03513ec7",
	"disc-any-02.jpg":      "c0ad2713a6e7f2e2aac33618512f92b2a7b64346bcb4f7f174a17b8654546cc9",
	"film-any-01.jpg":      "cd7aff1305768c114c22e966bf655e9a2b0518182e3ea6aeb1b29a36b2020b3b",
	"film-any-02.jpg":      "02808be30afb9b8e5ef252d26fe31b09b17cde5c41c5305741903f05e745ac7b",
	"film-any-03.jpg":      "b8439b1acd786122715f6ea4b8618bf380b6c5e8dbc3f7b02bace22e0c982be1",
	"flatlay-any-01.jpg":   "d9537154a3d8bcfbe61dea47d509c8cb2724f410ff31f7541ed152d48b2da8f2",
	"flatlay-any-02.jpg":   "7f6324246b5b7e9eccc76a5ddadd4364f8740b5bc65485654fb277ab3c9b0290",
	"flatlay-any-03.jpg":   "ff7a888837e1465ce55deb9745a41d847437c3ca94653f055b1c6c4ffbe2e202",
	"forest-dawn-01.jpg":   "e4efbac77a829923fe6b965a47c4df6ad012ce74b581bd803593b56d19fe2aed",
	"forest-dawn-02.jpg":   "5c33205895c2a19f99d8af1208209e43c5859ff4775e265f30f20db41209c89d",
	"forest-day-01.jpg":    "f9ea65d03f0b2112aedb6363d44068ed2b7dc38d0e0664b0df96068f904375ab",
	"forest-day-02.jpg":    "cd469992f9368fd866fedca8b8580345bd8df9ffdc905f3792cb9ebeea08cbd0",
	"forest-dusk-01.jpg":   "83ae3cc99877adb2cd19642900854841880f4b1278ead660556b84c760175a41",
	"harbor-day-01.jpg":    "d3db3b50188c5231819a9af03c87cd0d63f294847a597bcb9eab1778663d299a",
	"harbor-day-02.jpg":    "9b52f68b7cc252ca8ab77012917a35362c7193371068be02cd72f7dd16659970",
	"harbor-dusk-01.jpg":   "f54034a2f3039c80d7fd2915aeadd2e46443f08c79e40ab32183f15dca776d56",
	"harbor-dusk-02.jpg":   "58c6c9e81221373da8e10d4175d971e3a5c01d239960a628b4775ad5d476fbd0",
	"island-day-01.jpg":    "6c4ab672cad5271fba1fad2d26d28772b4bcaeefbb0800ef91563d85f76711d2",
	"island-day-02.jpg":    "9f436e4e4ec2d18a281cb91d54352c8b01e8c907137abfceafc0ead1cdbadc2e",
	"island-day-03.jpg":    "53580720c76ce6082712378b72c8334a18ec0171df62b2effcf84d73041b114f",
	"island-day-04.jpg":    "25b56adae67a3f69affc959236238886e0c4c066226dae03e64227dd2f9771cc",
	"lake-dawn-01.jpg":     "a45014417b48f35d7c3ab9b75ca7ded28554eb3c7753c788d8aa3590691f2281",
	"lake-dawn-02.jpg":     "1eab833e2ed6b4fe83faba20c691c47dfc5cc8dca8dd5b2c8a4b564251212fa7",
	"lake-day-01.jpg":      "7e3b5ce472e5ee283c757fc81dd137814bacd0d6c48c6e47787d16af07a47cb1",
	"lake-day-02.jpg":      "f76febbdec228c76e1a17cac55764219b435884b4598f4d553ea5a4492f1d650",
	"lake-dusk-01.jpg":     "1c3ec4ce7b517eb65286055752d7af4ad2bc7c60612e39f6f79df67fe4af3228",
	"market-dawn-01.jpg":   "ae3e6913d91596b2ce6a1f3283e153c26d6a1e6559199253708d672132ba01ce",
	"market-dawn-02.jpg":   "06de6407271e10ba0ccc56b39f54ebf71858b6bb75d8a7812a9104e069656c0a",
	"market-day-01.jpg":    "78ab17f6489e40eb884080baa327db675a44be89f2facf8c12edd21ac5e306e0",
	"market-day-02.jpg":    "da34ab0aa121d23cc67237f4ad570d44f20b6331939e3f3cb4fb644511dd1c61",
	"mountain-dawn-01.jpg": "4cf6a328fa21be793b39553a02fc99c5ea50180236ec1d8b76407380a535802c",
	"mountain-dawn-02.jpg": "164dfcd0e51ba26dc44753182494dab0423baf4d678db852abc364f677fb2e52",
	"mountain-day-01.jpg":  "fc58080000d0e405efaeb552f8967438870ead4f9956df66b8ea9ba6eea6ed37",
	"mountain-day-02.jpg":  "1767853500745ee48ee5e59609940fc969077b744b1f42e20be82869a84f822e",
	"mountain-dusk-01.jpg": "947000216a6cc530ed21e930fa77850d8ccd6d9f696ba522de4f4382ed3daa28",
	"mountain-dusk-02.jpg": "91313f7b94f80a447420eb655b316118daa081add7e56aba140806176b75bce9",
	"notebook-any-01.jpg":  "2d262361831c7ac01da6d8c8256aa08312b8e04d3dc29a6592afdb0f9ec57785",
	"notebook-any-02.jpg":  "2b2f7cd3b78f44eb3e871ed314de5285ac13905c95e40f24628eab418fe6968f",
	"notebook-any-03.jpg":  "74bab39533b1961bcfd55eba4ed18e915b30b813629e92629e8a0e20d3354f1c",
	"prints-any-01.jpg":    "7b009bbfedc515102fe0e98d6593b00399aca57de0257ccf3fecca60dd74fb02",
	"prints-any-02.jpg":    "9b9eba5a37a16dc01e8ad5a535cb0159166b2e34b82ebd19f62199d19d1a19b0",
	"prints-any-03.jpg":    "608db2a35bacc8217d53f49cf9d1fdcc678ce6d38716e31b76491c575cda2884",
	"rack-any-01.jpg":      "a0eac5dae5e5d52470109fd6a2bd79ffaaf974e626b0094c10cc8a44a31e3d0d",
	"rack-any-02.jpg":      "bdca124515bb234e71c4e31a609fd3520ea4e839c154c31d71b060a49fac17ce",
	"rack-any-03.jpg":      "bef0a506cc42fe1385e2179c24db0cdc853719da02ea9466aab5b78451772d6e",
	"sea-dawn-01.jpg":      "309963670eadeb223b589cf6a1c68d1436ecc1a3c4d04ba339daf2546d3109d6",
	"sea-dawn-02.jpg":      "e66f433b4dcf3570620dcd1c300c7b3dca414fcd7a483bf2e4fdd04b6693ecb9",
	"sea-day-01.jpg":       "a847b3bc324cd6cb03c7d292a98c24d5fadc59f8d6e137e262f1b89df0e9ad02",
	"sea-day-02.jpg":       "a12209cc1e541d3157b6102653b03f610dfed728dfa9e41b125f26a2bfce703e",
	"sea-dusk-01.jpg":      "9d711b3f415f501992527d3484f2c6b36b95134fb6fcb0bddb2fca75ae38623c",
	"sea-dusk-02.jpg":      "58ec4dc9665b40bd8716034e59883b82f579d7e85f925c407ffcf824cc5fc654",
	"studio-any-01.jpg":    "c7a8b589133e3613bfc74b32b2e05cfd672894368076a749cb892cbb0422a91d",
	"studio-any-02.jpg":    "f353e969ec05cdc575d4ad8dd74b4927ac9adedfc25b7ccbaa53269149cf0d58",
	"studio-any-03.jpg":    "41c1d0ff7e181593b166b9b3188ab427baf5825567e93de7c0680813cc9955d5",
	"studio-any-04.jpg":    "761e6f11838f0ef3896b8f2c8a30952c45a5da671e159a1a9e17c5c6af271f2e",
	"studio-any-05.jpg":    "e293b9472127fcacf2abb1cac1a3e61910af79f6db30e474196d3f53d4c7228a",
	"studio-any-06.jpg":    "57f9ae3a4a09009ecb8bca0785b33f6a5affe0a5757ecbf15602c0bcdbde0ecf",
	"town-day-01.jpg":      "f8c6d1685dffa52472a8e99eb9898beefa0800fba51d0b47070e7b25b5c857ef",
	"town-day-02.jpg":      "122ef388bcc257d04eccd95daa59d6a223366db2035a70e64f5b38094f9e0726",
	"town-dusk-01.jpg":     "a84e5bbd253fe011a400e7cb76a5e0d171ca363d337f932cb86ee36fff188b1e",
	"town-rain-01.jpg":     "c754d428023c65c4cba31c7f40ee4891892fea12d3a8eee510f70fe29e2bdd5a",
	"town-rain-02.jpg":     "b445281e7f6b41760d32e7738058bf943e90b81ef9810a109fccca8e7daf0095",
	"train-day-01.jpg":     "5594c9e123854aee595a5486b1bc040c854f6ccbba974d26b66f18587dd5a802",
	"train-day-02.jpg":     "a71aeb6fcef222e83ade99b118a905c59c7cd8e074c05015dae487c112b90d2f",
	"train-night-01.jpg":   "c9e9f8202993360c9d77b6efed61c1b3e58dd6ef8884b2199b935dd24571d10c",
	"train-night-02.jpg":   "3f69915177d3f0b3dd9e669a6f23ff09531a29d0477a037c1ddd8f42a8216573",
}

// Photos 处理好的照片(按本机种子裁切调色后的 JPEG)。还没有的返回 nil,生成器照旧画插图。
type Photos interface {
	Photo(name string, w, h int) []byte
}

// PhotoReq 生成时用到、但还没处理好的一张照片(某个尺寸)
type PhotoReq struct {
	Name string
	W, H int
}

type mediaEntry struct{ name, tod string }

// mediaByKind 类别 → 这一类的照片(按文件名排好,同一种子挑出同一张)
var mediaByKind = func() map[string][]mediaEntry {
	m := map[string][]mediaEntry{}
	names := make([]string, 0, len(MediaFiles))
	for n := range MediaFiles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		parts := strings.Split(strings.TrimSuffix(n, ".jpg"), "-")
		if len(parts) != 3 {
			continue
		}
		m[parts[0]] = append(m[parts[0]], mediaEntry{n, parts[1]})
	}
	return m
}()

// photoFor 给一个封面位挑照片:先找同类别同天色的(物件照不看天色),没有就同类别的任意一张;同一个站里尽量不重样。
// 用自己的哈希(种子、位置、类别),不动生成器的主随机数。没有这一类照片时返回空(画插图)。
func photoFor(seed uint64, slot int, kind, tod string, used map[string]bool) string {
	all := mediaByKind[kind]
	if len(all) == 0 {
		return ""
	}
	var cands []mediaEntry
	for _, e := range all {
		if e.tod == "any" || tod == "" || e.tod == tod {
			cands = append(cands, e)
		}
	}
	if len(cands) == 0 {
		cands = all
	}
	h := fnv.New64a()
	var b [8]byte
	for i := range b {
		b[i] = byte(seed >> (8 * i))
	}
	_, _ = h.Write(b[:])
	_, _ = h.Write([]byte{byte(slot), byte(slot >> 8)})
	_, _ = h.Write([]byte(kind))
	start := int(h.Sum64() % uint64(len(cands)))
	for i := range cands {
		if e := cands[(start+i)%len(cands)]; !used[e.name] {
			return e.name
		}
	}
	return cands[start].name
}

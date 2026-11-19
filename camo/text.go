package camo

// 伪装站上的文字(视频标题与简介、文章标题与正文、关于页、软件功能介绍)按种子拼出来,不用固定的句子。
// 所有 m-ui 共用这一份代码:写死的句子随便拿一句去搜,就能找出所有伪装站(用户 11-18 要求细查时发现的)。
// 每个位置有几种说法,句子里还有 {甲|乙|丙} 这样的可选词,拼出来的组合成千上万,各台的文字很少重样。

import (
	"fmt"
	"strings"
)

// alt 展开句子里的 {甲|乙|丙}:每一处随机挑一个(不嵌套)
func (g *gen) alt(s string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '{')
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString(g.pick(strings.Split(s[i+1:i+j], "|")))
		s = s[i+j+1:]
	}
}

// say 一句话的几种说法里挑一种,再展开里面的 {甲|乙}。站上凡是给人看的句子都经它(或 alt)出来:
// 各台用同一句的话,拿这一句去搜就能把所有伪装站找出来(审查 F-H1:视频站页脚、404 页、镜像首页等原来各台一字不差)。
// 一个站里要用几次的说法(「登录」「会员专享」这类)在生成这个站时先挑好,前后一致。
func (g *gen) say(variants ...string) string { return g.alt(g.pick(variants)) }

// title 页面标题:页名和站名怎么连、谁在前按种子定(sep、siteFirst)
func (g *gen) title(page, site string) string {
	if page == "" {
		return site
	}
	if g.sk.siteFirst {
		return site + g.sk.sep + page
	}
	return page + g.sk.sep + site
}

// fill 把 args 填进 format;format 里没有占位符的原样返回。几种说法里常有一两种用不上参数,
// 直接 Sprintf 会在后面多出一截 %!(EXTRA …)。要用到不止一个参数的说法一律写成 %[1]s 这样带序号的。
func fill(format string, args ...any) string {
	if !strings.Contains(format, "%") {
		return format
	}
	return fmt.Sprintf(format, args...)
}

func capFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---- 视频 ----

// videoPlaces 视频的地点、封面画什么(图包的类别),和地点本身限定的天色(空 = 不限):
// 冰湖只有白天那两张,「河边」是黄昏的河,「屋顶」是黄昏的屋顶,「夜车」「城市灯火」是夜里的……
var videoPlaces = []struct{ name, kind, tod string }{
	{"Coastline", "sea", ""}, {"Harbor", "harbor", ""}, {"Old Town", "town", ""}, {"City", "city", ""}, {"Mountain Trail", "mountain", "day"},
	{"Forest", "forest", ""}, {"Desert Road", "desert", "day"}, {"Lake", "lake", "dawn"}, {"Island", "island", ""}, {"Market", "market", ""},
	{"Rooftop", "city", "dusk"}, {"Night Train", "train", "night"}, {"Valley", "mountain", ""}, {"Pine Woods", "forest", ""}, {"Fishing Village", "harbor", ""},
	{"Old Streets", "town", ""}, {"Summit", "clouds", ""}, {"Bay", "sea", ""}, {"Old Railway", "train", "day"}, {"Dunes", "desert", "dusk"},
	{"Frozen Lake", "lake", "day"}, {"High Pass", "clouds", ""}, {"Headland", "sea", ""}, {"Riverside", "lake", "dusk"}, {"City Lights", "city", "night"},
}

// videoTimes 标题后半截:按封面(照片)的天色挑;tod 为空的哪种天色都能用
var videoTimes = []struct{ text, tod string }{
	{"at First Light", "dawn"}, {"at Dawn", "dawn"}, {"in the Morning", "dawn"}, {"at Sunrise", "dawn"},
	{"at Golden Hour", "dusk"}, {"at Dusk", "dusk"}, {"Sunset", "dusk"}, {"in the Evening", "dusk"},
	{"After Dark", "night"}, {"by Night", "night"}, {"at Night", "night"},
	{"on a Clear Day", "day"}, {"in the Sun", "day"}, {"Midday Walk", "day"},
	{"in the Rain", "rain"}, {"on a Rainy Day", "rain"},
	{"Timelapse", ""}, {"Walk", ""}, {"in 4K", ""}, {"Ambience", ""}, {"Slow Tour", ""},
}

// studioTitles 棚内视频的标题,按图包里那张照片拍的是什么:录音棚、摄影 / 片场布光、手工作坊
var studioTitles = map[string][]string{
	"music": {"Studio Session", "Session Tape", "Room Recording", "Live Rehearsal", "Late Night Session"},
	"set":   {"Behind the Scenes", "On Set", "Lighting Test", "Set Walkthrough"},
	"shop":  {"Workshop Recording", "At the Bench", "Making Of", "Workshop Diary"},
}

func studioKind(photo string) string {
	switch {
	case strings.HasPrefix(photo, "studio-any-03"), strings.HasPrefix(photo, "studio-any-04"):
		return "set"
	case strings.HasPrefix(photo, "studio-any-05"), strings.HasPrefix(photo, "studio-any-06"):
		return "shop"
	}
	return "music"
}

// todOf 图包文件名里的天色(any 和取不到时返回空)
func todOf(photo string) string {
	parts := strings.Split(strings.TrimSuffix(photo, ".jpg"), "-")
	if len(parts) == 3 && parts[1] != "any" {
		return parts[1]
	}
	return ""
}

// videoPick 一个视频:先挑地方,再按地方挑封面照片,标题后半截按照片的天色起(标题和封面对得上);
// 棚内视频的标题按照片拍的是什么起。同一个站里标题不重样。没有照片的类别照样按这个挑插图的天色。
func (s *Site) videoPick(g *gen, slot int, titles map[string]bool) (title, kind, tod, photo string) {
	for i := 0; ; i++ {
		if g.r.IntN(6) == 0 {
			kind, tod = "studio", ""
			photo = photoFor(g.seed, slot, kind, "", s.used)
			title = g.pick(studioTitles[studioKind(photo)])
			if g.r.IntN(2) == 0 {
				title += fmt.Sprintf(" No. %d", g.n(2, 24))
			}
		} else {
			p := videoPlaces[g.r.IntN(len(videoPlaces))]
			kind, photo = p.kind, photoFor(g.seed, slot, p.kind, p.tod, s.used)
			if tod = todOf(photo); tod == "" {
				if tod = p.tod; tod == "" {
					tod = g.pick([]string{"dawn", "day", "dusk"})
				}
			}
			var opts []string
			for _, t := range videoTimes {
				if (t.tod == "" || t.tod == tod) && !sharesWord(p.name, t.text) {
					opts = append(opts, t.text)
				}
			}
			title = p.name + " " + g.pick(opts)
		}
		if !titles[title] || i > 50 {
			titles[title] = true
			return title, kind, tod, photo
		}
	}
}

// sharesWord 两段文字有没有同一个词(「Night Train」不配「by Night」)
func sharesWord(a, b string) bool {
	for _, w := range strings.Fields(strings.ToLower(a)) {
		for _, v := range strings.Fields(strings.ToLower(b)) {
			if len(w) > 3 && w == v {
				return true
			}
		}
	}
	return false
}

// newVideoNotes 视频简介:每个站先从各段的说法里各挑两三种,这个站的十二个视频就在这几种里拼。
// 原来每段四五种说法、十二个视频各拼一次,结果每种说法几乎每个站都有(「with the colours left close to how they looked」九成的站都有)。
func newVideoNotes(g *gen) func() string {
	verbs := g.pickN([]string{"Recorded", "Filmed", "Shot", "Captured", "Made", "Put together", "Photographed", "Taped"}, 2)
	hows := g.pickN([]string{"on location", "over two days", "in a single take", "handheld", "on a quiet weekday", "early in the season", "over a long weekend", "between showers",
		"with one camera and a tripod", "from first light to dusk", "in one afternoon", "on the last warm day of the year", "without a script", "on a borrowed camera"}, 3)
	sounds := g.pickN([]string{"with the original ambient sound", "with no narration and no music", "with just the sound of the place", "and edited down to the quiet moments",
		"with the colours left close to how they looked", "with nothing added in the edit", "and cut as little as possible", "with natural sound only", "and left mostly as it was filmed",
		"with a light grade and no music", "with the wind left in", "and trimmed to the best part"}, 3)
	extras := append(g.pickN([]string{" Best watched full screen.", " Headphones recommended.", " Part of a series we add to every few weeks.", " Shot on a small camera, nothing fancy.",
		" Turn the lights down for this one.", " Good company for a slow evening.", " No talking, no captions.", " One of our favourites from this year.",
		" Filmed for members who asked for more of this place.", " Works well on a second screen.", " Subtitles are not needed."}, 3), "", "")
	return func() string { return g.pick(verbs) + " " + g.pick(hows) + " " + g.pick(sounds) + "." + g.pick(extras) }
}

// ---- 博客 ----

// blogTopics 文章的题目、题图和标签
var blogTopics = []struct {
	text, kind string
	tags       []string
}{
	{"long exposures at dusk", "sea:dusk", []string{"photography", "evenings"}}, {"cycling the coast", "sea:day", []string{"cycling", "travel"}},
	{"rebuilding my home server", "rack", []string{"homelab", "notes"}}, {"a hundred rolls of film", "film", []string{"film", "photography"}},
	{"packing light for a month", "desert:dusk", []string{"travel", "gear"}}, {"the small tools I use every day", "flatlay", []string{"gear", "desk"}},
	{"printing my photos again", "prints", []string{"photography", "prints"}}, {"walking the old railway line", "train:day", []string{"walking", "history"}},
	{"keeping a notebook", "notebook", []string{"notebooks", "habits"}}, {"repairing my own bike", "bike", []string{"cycling", "repair"}},
	{"a quiet weekend in the mountains", "mountain:dusk", []string{"hiking", "travel"}}, {"burning photos to disc", "disc", []string{"backups", "photography"}},
	{"a winter by the lake", "lake:day", []string{"winter", "walking"}}, {"night walks in the city", "city:night", []string{"walking", "photography"}},
	{"camping in the pine woods", "forest:dawn", []string{"camping", "outdoors"}}, {"island hopping on a budget", "island:day", []string{"travel", "budget"}},
	{"sunrise above the clouds", "clouds:dawn", []string{"hiking", "photography"}}, {"slow mornings at the market", "market:dawn", []string{"food", "travel"}},
	{"walking the old town in the rain", "town:rain", []string{"walking", "weather"}}, {"photographing the harbor before work", "harbor:day", []string{"photography", "mornings"}},
	{"taking the night train", "train:night", []string{"travel", "trains"}}, {"shooting from rooftops", "city:dusk", []string{"photography", "city"}},
	{"rearranging my desk", "flatlay", []string{"desk", "habits"}}, {"scanning old negatives", "film", []string{"film", "archive"}},
	{"moving everything to a small rack", "rack", []string{"homelab", "hardware"}}, {"sketching in a pocket notebook", "notebook", []string{"notebooks", "drawing"}},
	{"riding before breakfast", "bike", []string{"cycling", "mornings"}}, {"crossing the desert by bus", "desert:day", []string{"travel", "budget"}},
	{"taking the slow ferry", "island:day", []string{"travel", "sea"}}, {"getting up for first light on the ridge", "mountain:dawn", []string{"hiking", "photography"}},
	{"walking in the fog", "forest:day", []string{"walking", "photography"}}, {"swimming in the lake at dawn", "lake:dawn", []string{"swimming", "mornings"}},
	{"evenings at the harbor", "harbor:dusk", []string{"photography", "evenings"}}, {"shopping at the weekday market", "market:day", []string{"food", "city"}},
	{"making a photo book", "prints", []string{"prints", "projects"}}, {"archiving ten years of photos", "disc", []string{"backups", "archive"}},
}

// blogTitlePatterns 文章标题的句式(一个站里不重样,见 genBlog);含直接用题目本身的那种,真人博客大多这样起
var blogTitlePatterns = []string{"Notes on %s", "What I learned from %s", "A few thoughts on %s", "Lessons from %s", "Revisiting %s", "Some notes on %s",
	"%s", "On %s", "%s, revisited", "Field notes: %s", "%s: what worked", "Things I got wrong about %s"}

// blogLeads 文章开头一句的句式。每一句里处处有可选词:首页上列着八篇文章的开头,句式里固定不变的一截(比如
// 「There is not much written about」)各台都一样的话,拿那一截去搜就能找出一半的博客(审查 F-H1,源码片段分析)。
var blogLeads = []string{
	"I {have been|had been|was} {thinking about|meaning to try|putting off|curious about} %s for {a while|months|ages|most of a year}, and {this autumn|this spring|last month|a few weeks ago} I {finally|at last} {made time for it|got started|gave it a go}.",
	"{This|It} {started|began} as {a small experiment|an experiment|a side project|a weekend idea} with %s and {slowly|somehow|quietly|gradually} {turned into|became} {a habit|a routine|part of my week}.",
	"{Getting into|Starting with|Picking up} %s was {supposed|meant} to take {a weekend|a few days|an afternoon}. It took {most of the season|the better part of a year|three months|far longer}, and I {do not regret it|would do it again|am glad it did}.",
	"{A few people|Some friends|Two readers|A couple of people} {asked me about|wanted to know about|wrote to me about} %s, so {here is|this is} {everything|most of what|what} I {wish I had known|would tell myself} {at the start|a year ago|before starting}.",
	"I {went back to|returned to|picked up} %s {this year|again this year|after a long break} with {fewer expectations|a much simpler plan|less gear and more time|no plan at all}.",
	"{Most|Nearly all|Almost everything} of what I know about %s I {learned|picked up|worked out} by getting it wrong {first|a few times|more than once|repeatedly}.",
	"For {a while now|most of this year|the past few months}, %s has been {the thing|what|the one thing} I {look forward to|keep coming back to|plan my week around}.",
	"I {did not expect|never expected|was not expecting} %s to {change how I spend my weekends|teach me much|stick}, but {it did|here we are|it has}.",
	"{Here are|These are|Below are} {my notes|some notes|a few notes} on %s, {written|put together|typed up} {the same evening|while it is still fresh|a few weeks later|on the train home}.",
	"{Last month|Earlier this year|Over the holidays|A few weeks ago} I {finally|at last} {got around to|made time for|sat down with} %s.",
	"There is {not much|very little|hardly anything} {written|out there|online} about %s {from a beginner's point of view|that is not trying to sell something|in plain language}, so {here is my attempt|I wrote this|this is my try}.",
	"{Every autumn|Each spring|Every few months} I {tell myself|promise myself|decide} I will {get serious about|spend more time on|go back to} %s. {This time I did|This year it happened|It finally stuck}.",
	"{Nobody|No one} {warned|told} me how much {time|patience|space} %s {takes|would take|needs}.",
	"{A year ago|Last winter|Two years ago} I would {not|never} have guessed that %s would {matter this much to me|take over my weekends|end up on this site}.",
	"{Some notes|A short write-up|A long overdue post} {on|about} %s, {mostly|mainly|partly} so I {do not forget|remember|can find it again} {how it went|what worked|the details}.",
	"%s {is|turned out to be} {cheaper|easier|harder|slower} than {it looks|I thought|people say}, {and that is|which is} {most of the appeal|why I kept going|half the fun}.",
}

// blogLead 文章开头一句(列表里的摘要也是它)
func blogLead(g *gen, topic string) string {
	s := fmt.Sprintf(g.alt(g.pick(blogLeads)), topic)
	return capFirst(s)
}

var paraSentences = []string{
	"At first it was just an excuse to get {outside|out of the house|away from the screen}.",
	"None of this was {planned|very deliberate}, and {that was probably the point|I think that helped}.",
	"The hardest part was not the {equipment|planning|gear} but the {patience|waiting|time it takes}.",
	"Most of the work was {undoing my own earlier shortcuts|deciding what to leave out|starting over more than once}.",
	"I kept {the setup|everything} simple on purpose: fewer moving parts, fewer things to {break|forget|carry}.",
	"Light changes quickly near the end of the day, and the best {moments|frames} come right after you think it is over.",
	"Writing down what I actually used turned out to be more useful than any list I made {beforehand|in advance}.",
	"The weather did not cooperate, but the {grey|quiet|slow} days were kinder than I expected.",
	"A small notebook in my {pocket|bag} did more for me than any app.",
	"Looking back, the {small details|quiet mornings|mistakes} are what I remember most.",
	"If you are thinking about trying this, start {small|slowly|with one evening a week}.",
	"I will write a follow-up once I have {more to say|used it for a season|tried the next idea}.",
	"It works, and that is good enough {for now|for this year}.",
	"Next time I will {leave half of it at home|plan a little less|start earlier in the day}.",
	"It is worth the early alarm, {every time|almost every time}.",
	"There is a particular kind of quiet {on early mornings|out there|at that hour} that I have only found {outside the city|on my own}.",
	"Labeling {cables|boxes|everything} and writing a short note for every change would have saved me a {whole weekend|lot of guessing}.",
	"Nothing about it is clever, and I like it {that way|better for it}.",
	"I did not take many {pictures|notes} the first time, which I {regret|still think about}.",
	"The {second|third} attempt went better, mostly because I {slowed down|stopped rushing}.",
	"There is no {trick|secret} to it; you just have to {show up|keep going|do it again}.",
	"I {borrowed|bought second-hand} most of what I needed, and {it was plenty|that was the right call}.",
	"A friend told me to {keep a log|write things down as I go}, and it was the best advice I got.",
	"Some days {nothing works|it all falls apart}, and those are the days I learn {something|the most}.",
	"I used to overthink this kind of thing. {Now I mostly just start|These days I just begin}.",
	"It took me a {few weeks|couple of months} to stop comparing my results with other people's.",
	"The {cheap|simple|old} option turned out to be {fine|good enough|the one I still use}.",
	"I {made|drew} a small {checklist|map} on paper and {taped it to the wall|kept it in my bag}.",
	"{Half|Most} of the {fun|work} is in the {preparation|waiting}, as it turns out.",
	"I am still {surprised by|not used to} how {quiet|slow|empty} it gets after {sunset|the last bus leaves|the shops close}.",
	"If I {did it again|had to start over}, I would {bring less|spend less|ask for help sooner}.",
	"You notice {more|different things} when you {go alone|leave the phone in your bag|are not in a hurry}.",
	"I keep a {folder|box} of the failures too, because they are {more honest|a better record}.",
	"The {plan|route|schedule} changed {twice|three times} before I even left the house.",
	"{Coffee|Tea} first, then everything else; that part of the routine {never changes|is not negotiable}.",
	"My {hands|notes|shoes} were a mess by the end, and I {did not mind|was happy about it}.",
	"It is not {efficient|fast}, and I have {stopped|given up} trying to make it so.",
	"I {timed|measured} it once out of curiosity and then never again.",
	"The {people|neighbors|strangers} I met along the way were {kinder|more curious} than I expected.",
	"{Good shoes matter|Decent light matters|Enough sleep matters} more than {any|almost any} piece of gear.",
	"I {wrote|sketched} most of this {on the way back|the same evening|a week later}, while I still remembered the details.",
	"There were a few {wrong turns|false starts|bad ideas}, and I have left them in.",
	"The one thing I would not skip is {the test run|a slow first day|reading the manual}.",
	"By the {third|second} day I had stopped checking the {time|forecast|map}.",
	"I do not have a {conclusion|grand lesson}; it was {a good few days|worth doing}, and that is enough.",
	"The {best|nicest} part was {doing nothing for an hour|the walk back|how ordinary it felt}.",
	"It cost {a little|rather} more than I planned, mostly because of {my own mistakes|the small things}.",
	"I am {keeping|leaving} the setup as it is until {spring|the end of the year|something breaks}.",
	"{Small|Short} sessions, {often|every week}, worked better for me than {long weekends|big plans}.",
	"I took the long way {home|back} and do not regret it.",
	"{Somebody|Someone} asked what the point was. I {did not have|still do not have} a good answer.",
	"{Most|Half} of my {photos|notes} from that week are {blurry|unreadable}, and I am keeping them anyway.",
	"The {manual|guide} I found was from {2009|2012|another era}, and it was still {right|mostly right}.",
	"I gave myself one rule: {no new purchases|nothing that needs charging|finish before starting another}.",
	"It rained {the whole time|on the second day}, which {suited the mood|made it better, honestly}.",
	"{Everything|Most of it} fits in {one bag|a shoebox|a single drawer} now.",
	"I {plan|hope} to do it again {next year|in the autumn|when the weather turns}.",
	"For now, I am {happy|content} to {leave it there|call it done}.",
}

// paraBank 这个站的文章用句库里的哪些句子(24 句,生成这个站时挑一次)。每个站都用整个句库的话,
// 八篇文章下来库里每一句几乎每个站都有,句子里固定不变的那一截就成了各台共有的片段。
func (g *gen) paraBank() []int {
	if g.bank == nil {
		g.bank = g.r.Perm(len(paraSentences))[:24]
	}
	return g.bank
}

// blogParagraphs n 段正文,每段两三句,同一篇里的句子不重复
func blogParagraphs(g *gen, n int) []string {
	order := append([]int(nil), g.paraBank()...)
	g.r.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	var out []string
	k := 0
	for i := 0; i < n && k < len(order); i++ {
		m := g.n(2, 3)
		var ss []string
		for j := 0; j < m && k < len(order); j++ {
			ss = append(ss, g.alt(paraSentences[order[k]]))
			k++
		}
		out = append(out, strings.Join(ss, " "))
	}
	return out
}

// ---- 软件下载、镜像的说明 ----

// featureTexts 功能介绍:标题和说明都有几种说法
var featureTexts = [][2]string{
	{"{Fast|Quick} on {big|large} libraries", "Opens projects with {hundreds of thousands of|a great many} files in seconds, and stays {smooth|responsive} while you work."},
	{"Works offline", "Everything runs on your {computer|own machine}. Nothing is uploaded unless you {choose to share it|ask it to}."},
	{"Private by default", "No account {needed|required}, no ads, no tracking. Your files never leave your machine."},
	{"Gets out of the way", "A small, quiet window with keyboard shortcuts for {everything you do often|the things you do every day}."},
	{"{Every|Any} platform", "The same app on Windows, macOS and Linux, with settings that {follow you|move with you}."},
	{"Free updates", "Every release in the same major version is free, and older versions {stay available|remain online}."},
	{"{Light|Easy} on resources", "Idles at almost zero CPU and stays out of your way until you need it."},
	{"Keyboard first", "{Every|Almost every} action has a shortcut, and you can change the ones you don't like."},
	{"{Dark|Light and dark} mode", "Follows your system theme, or pick the one you prefer."},
	{"Open formats", "Your data stays in {plain|standard} files you can open with other tools."},
	{"{Automatic|Background} backups", "Changes are saved as you go, with {a week|thirty days} of history to roll back to."},
	{"Built for {long sessions|everyday use}", "Stable enough to leave open all week, with memory use that stays flat."},
	{"{Sync|Share} when you want", "Send a project to another device or a {colleague|friend} with one click, or keep it local."},
	{"No {sign-up|subscription}", "Download it, open it, use it. Paid plans are optional and never required."},
	{"Accessible", "Works with screen readers and respects your {contrast|text size} settings."},
	{"{Regular|Frequent} updates", "A new release every {few weeks|month or so}, with clear notes on what changed."},
}

func mirrorAbout(g *gen) string {
	p := func(variants ...string) string { return "<p>" + esc(g.say(variants...)) + "</p>" }
	return p("This mirror is run as a {public service|non-commercial service}. Projects are synchronized from their official upstream servers {several times a day|every few hours}, and files are served exactly as published.",
		"We run this mirror {in our spare time|as a service to the community}. Everything is pulled from the official upstream servers {several times a day|on a fixed schedule} and served unmodified.",
		"This server mirrors a selection of open-source projects. Content is synced from upstream {every few hours|several times a day}; we never change the files.",
		"The mirror is free to use and {has no affiliation with|is not run by} the projects it carries. Files are copied from the official servers as they are.",
		"This is a volunteer-run mirror. We sync {a few|several} times a day from each project's primary server and publish the files untouched.") +
		p("Please verify {downloads|what you download} with the checksums and signatures provided by each project. Bandwidth is shared, so {large|very large} or repeated downloads may be slower at {peak times|busy hours}.",
			"As with any mirror, check signatures and checksums before you install anything. Speeds can drop at {peak times|busy hours}.",
			"Always verify images against the project's own checksums. If a download is slow, try again {later|outside peak hours}.",
			"Checksums and detached signatures are mirrored alongside the files; please use them. There are no download limits, but be {considerate|reasonable}.",
			"Each directory carries the upstream checksum files. We ask heavy users to {limit parallel connections|keep parallel downloads low}.") +
		p("Access is available over HTTP and HTTPS only. There is no {rsync or FTP service|rsync, FTP or torrent service}.",
			"We serve HTTP and HTTPS. {rsync is not offered|There is no rsync access} at the moment.",
			"Only web access is provided for now; {rsync may follow later|there are no plans for FTP}.",
			"The mirror is reachable over IPv4 and IPv6, via HTTP and HTTPS.",
			"HTTP and HTTPS are supported. For rsync, please use {the upstream servers|another mirror}.")
}

func productAbout(g *gen, product string) string {
	p := func(variants ...string) string { return "<p>" + esc(fill(g.say(variants...), product)) + "</p>" }
	return p("%s is a {small|lightweight} desktop app for working with large files on {any computer|the computer you already have}. Downloads are served from our own servers; please use the checksums listed with each release to verify them.",
		"We make %s for people who want a {fast, quiet|simple, reliable} tool that does one job well. Installers are hosted here, with checksums next to every file.",
		"%s started as an internal tool and grew from there. It runs on Windows, macOS and Linux and is distributed from this site only.",
		"%s is built by a small team. Every release is published here first, together with its SHA-256 checksums.",
		"The idea behind %s is simple: {open fast|start quickly}, stay out of the way, and never lose your work. All downloads on this site are official builds.") +
		p("Older releases stay available for people who need them, but only the latest version {receives fixes|gets updates}.",
			"Previous versions remain online. Security fixes go into the current release only.",
			"We keep every release in the archive, and recommend the latest one for {most people|everyday use}.",
			"If an update causes trouble, earlier builds can be found in the release list.",
			"Only the newest version is supported, but older installers are kept for {compatibility|older systems}.")
}

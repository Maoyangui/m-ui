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

func capFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---- 视频 ----

// videoPlaces 视频的地点和它的封面画什么
var videoPlaces = []struct{ name, kind string }{
	{"Coastline", "sea"}, {"Harbor", "harbor"}, {"Old Town", "town"}, {"City", "city"}, {"Mountain Trail", "mountain"},
	{"Forest", "forest"}, {"Desert Road", "desert"}, {"Lake", "lake"}, {"Island", "island"}, {"Market", "market"},
	{"Rooftop", "city"}, {"Night Train", "train"}, {"Valley", "mountain"}, {"Pine Woods", "forest"}, {"Fishing Village", "harbor"},
	{"Canal", "town"}, {"Summit", "clouds"}, {"Bay", "sea"}, {"Old Railway", "train"}, {"Dunes", "desert"}, {"Frozen Lake", "lake"},
	{"High Pass", "clouds"}, {"Lighthouse", "sea"}, {"Riverside", "lake"},
}

// videoTimes 标题后半截:什么时候、什么样的片子;tod 决定封面的天色(空 = 随便)
var videoTimes = []struct{ text, tod string }{
	{"at First Light", "dawn"}, {"at Dawn", "dawn"}, {"in the Morning", "dawn"}, {"at Golden Hour", "dusk"}, {"at Dusk", "dusk"},
	{"After Dark", "night"}, {"by Night", "night"}, {"Timelapse", ""}, {"Walk", ""}, {"in 4K", ""}, {"Ambience", ""}, {"Drive", "day"},
	{"Sunset", "dusk"}, {"in Winter", "day"}, {"Slow Tour", ""}, {"in the Rain", "rain"}, {"on a Clear Day", "day"},
}

var studioTitles = []string{"Studio Session", "Behind the Scenes", "Workshop Recording", "Live Rehearsal", "Session Tape", "Late Night Session", "Room Recording"}

// videoTitle 一个视频的标题和封面的样子(同一个站里不重样)
func videoTitle(g *gen, used map[string]bool) (title, kind, tod string) {
	for i := 0; ; i++ {
		if g.r.IntN(6) == 0 {
			title, kind, tod = g.pick(studioTitles), "studio", ""
			if g.r.IntN(2) == 0 {
				title += fmt.Sprintf(" No. %d", g.n(2, 24))
			}
		} else {
			p, t := videoPlaces[g.r.IntN(len(videoPlaces))], videoTimes[g.r.IntN(len(videoTimes))]
			title, kind, tod = p.name+" "+t.text, p.kind, t.tod
			if tod == "" {
				tod = g.pick([]string{"dawn", "day", "dusk", "night"})
			}
		}
		if !used[title] || i > 50 {
			used[title] = true
			return title, kind, tod
		}
	}
}

func videoNote(g *gen) string {
	return g.alt("{Recorded|Filmed|Shot|Captured} {on location|over two days|in a single take|handheld|on a quiet weekday|early in the season} " +
		"{with the original ambient sound|with no narration and no music|with just the sound of the place|and edited down to the quiet moments|with the colours left close to how they looked}." +
		"{| Best watched full screen.| Headphones recommended.| Part of a series we add to every few weeks.| Shot on a small camera, nothing fancy.}")
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
}

var blogTitlePatterns = []string{"Notes on %s", "What I learned from %s", "A few thoughts on %s", "Lessons from %s", "Revisiting %s", "Some notes on %s"}

var blogLeads = []string{
	"I have been {thinking about|meaning to try|putting off} %s for a while, and {this autumn|this spring|last month} I finally made time for it.",
	"This {started|began} as a small experiment with %s and {slowly|somehow} turned into a habit.",
	"Getting into %s was supposed to take a weekend. It took {most of the season|the better part of a year|three months}, and I do not regret it.",
	"A few people asked me about %s, so here is {everything|most of what} I wish I had known at the start.",
	"I went back to %s this year with {fewer expectations|a much simpler plan|less gear and more time}.",
	"Most of what I know about %s I learned by getting it wrong {first|a few times|more than once}.",
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
}

// blogParagraphs n 段正文,每段两三句,同一篇里的句子不重复
func blogParagraphs(g *gen, n int) []string {
	order := g.r.Perm(len(paraSentences))
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
}

func mirrorAbout(g *gen) string {
	return "<p>" + g.alt("This mirror is run as a {public service|non-commercial service}. Projects are synchronized from their official upstream servers {several times a day|every few hours}, and files are served exactly as published.") +
		"</p><p>" + g.alt("Please verify {downloads|what you download} with the checksums and signatures provided by each project. Bandwidth is shared, so {large|very large} or repeated downloads may be slower at {peak times|busy hours}.") +
		"</p><p>" + g.alt("Access is available over HTTP and HTTPS only. There is no {rsync or FTP service|rsync, FTP or torrent service}.") + "</p>"
}

func productAbout(g *gen, product string) string {
	return "<p>" + fmt.Sprintf(g.alt("%s is a {small|lightweight} desktop app for working with large files on {any computer|the computer you already have}. Downloads are served from our own servers; please use the checksums listed with each release to verify them."), product) +
		"</p><p>" + g.alt("Older releases stay available for people who need them, but only the latest version {receives fixes|gets updates}.") + "</p>"
}

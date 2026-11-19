package camo

// 视频站(会员制片库,用户 11-19 精修):首页一张大的精选,下面是片库;播放页左边播放器、右边「接下来」;
// 点播放先转一会儿圈,再说只给会员看,给「登录」「注册」两个按钮。顶栏有「登录」。
// 登录、注册(要邀请码 —— 实际没有,怎么填都不通过,只为把注册挡住)、找回密码三页,表单经接口提交(api.go):
// 开着脚本时是页面里的脚本发 JSON,没开时表单直接提交、回一页带提示的同一页。还有当前用户(一律没登录)、
// 视频列表 / 详情、播放(一律要先登录)几个接口。封面有图包照片就用照片(media.go),没有画插图。
//
// 页面上给人看的句子、按钮上的字、表单的提示都按种子挑说法(text.go 的 say),「登录」「注册」「会员专享」这几个
// 一个站里到处要用的词先挑好、前后一致。原来页脚那句各台一字不差,内页有 28 句也是(审查 F-H1)。

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// authPaths 登录、注册、找回密码三页的地址和对应的接口(相对接口前缀;按种子挑一套常见的写法)
type authPaths struct{ login, register, reset, apiLogin, apiRegister, apiReset string }

func pickAuthPaths(g *gen) authPaths {
	pages := [][3]string{{"/login", "/register", "/forgot-password"}, {"/signin", "/signup", "/password/reset"},
		{"/account/login", "/account/register", "/account/recover"}, {"/sign-in", "/join", "/reset-password"},
		{"/members/login", "/members/join", "/members/reset"}, {"/auth/login", "/auth/signup", "/auth/forgot"}}
	apis := [][3]string{{"auth/login", "auth/register", "auth/password/forgot"}, {"session", "users", "password/reset"},
		{"login", "signup", "password-reset"}, {"auth/signin", "auth/signup", "auth/recover"},
		{"sessions", "accounts", "passwords"}, {"account/login", "account/create", "account/recover"}}
	p, a := pages[g.r.IntN(len(pages))], apis[g.r.IntN(len(apis))]
	return authPaths{p[0], p[1], p[2], a[0], a[1], a[2]}
}

// jsonKey 接口里字段名的写法(按种子:snake_case 或 camelCase)
func (g *gen) jsonKey(k string) string {
	if !g.sk.camel {
		return k
	}
	return jsCamel(strings.ReplaceAll(k, "_", "-"))
}

// videoCats 首页那一排分类(第一个是「全部」,另按种子挑几个)
var videoCats = []string{"Travel", "City", "Nature", "Studio", "Walks", "Coast", "Mountains", "Night", "Sessions", "Ambience", "Timelapse",
	"Trains", "Markets", "Forests", "Islands", "Slow TV", "Sunrise", "Rain", "Harbors", "Roads"}

// linked 一句中间带一个链接的话:前半句、链接上的字、后半句
type linked struct{ pre, link, post string }

func genVideo(s *Site, g *gen) {
	s.name = g.pick(words) + " " + g.pick([]string{"Video", "Stream", "Screen", "Reels", "Films", "TV", "Cinema", "Channel", "Motion", "Pictures", "Play", "Vision"})
	what := g.say("travel films, city walks and studio sessions", "long recordings, travel films and studio sessions", "quiet travel films and studio recordings",
		"slow films from the road and the studio", "walking tours, landscapes and live sessions", "unhurried films: places, weather and music", "ambient films and live recordings")
	s.desc = s.name + fmt.Sprintf(g.say(" is a members-only library of %s.", ": %s for our members.", " streams %s to members.", " — %s.", " hosts %s for a small community.", ". A private library of %s."), what)
	s.logo = `<span class="` + g.c("logo") + ` ` + g.c("play") + `"></span>`
	s.foot = `<span>` + esc(g.say("Streaming for members in our apps", "Member-supported and ad-free", "Made for slow evenings", "Independent, with no ads", "New recordings every week",
		"Films for members, on every screen", "No ads, no autoplay", "Thanks for watching", "Supported by our members", "Watch on the web, TV and mobile")) + `</span>`
	s.apiPre = g.sk.api
	ap := pickAuthPaths(g)
	inviteKey := g.pick([]string{"invite", "invite_code", "inviteCode", "code"})
	dot := g.sk.dot

	// 这个站里到处要用的几个词,先挑好
	in := g.pick([]string{"Sign in", "Log in"})
	inL := strings.ToLower(in)
	up := g.say("Create account", "Sign up", "Join", "Register", "Create an account", "Get started")
	members := g.say("Members only", "For members", "Members", "Member access", "Subscribers only", "Members' library")
	viewFmt := g.pick([]string{"%s views", "%s views", "%s plays", "%s streams", "Watched %s times", "%s watches"})
	views := func(n int) string { return fmt.Sprintf(viewFmt, fmtViews(n)) }
	codeNoun := g.pick([]string{"invite code", "invitation code", "invite", "access code", "invite key"})
	mailLabel, passLabel := g.say("Email", "Email address", "Your email", "E-mail", "Email"), g.say("Password", "Password", "Your password", "Password")

	s.cta = fmt.Sprintf(`<a class="%s" href="%s">%s</a>`, g.c("cta"), ap.login, esc(g.pick([]string{in, in, "Members", "Account", "Login"})))
	s.addScript(g, scriptParts{forms: true, me: "me", play: true, formKey: g.sk.formKey, playKey: g.sk.playKey})
	homeNav := g.say("Library", "Videos", "Browse", "Watch", "Home", "All videos", "Films", "Latest")
	aboutNav := g.say("About", "About", "About us", "Info", "Membership")
	nav := [][2]string{{"/", homeNav}, {"/about/", aboutNav}}

	type vid struct {
		id, title, dur, date, thumb, big, wide string
		secs, views                            int
		at                                     time.Time
	}
	var vids []vid
	tw, th, bw, bh, ww, wh := g.sk.thumb[0], g.sk.thumb[1], g.sk.big[0], g.sk.big[1], g.sk.wide[0], g.sk.wide[1] // 几种图的尺寸(按种子)
	used := map[string]bool{}
	day := g.n(1, 5)
	for i := 0; i < 12; i++ {
		ti, kind, tod, photo := s.videoPick(g, i, used)
		v := vid{id: g.id(11), title: ti, secs: g.n(2, 48)*60 + g.n(0, 59), views: viewCount(g)}
		v.dur = fmt.Sprintf("%d:%02d", v.secs/60, v.secs%60)
		v.at = g.base.Add(-time.Duration(day)*24*time.Hour + time.Duration(g.n(7, 21))*time.Hour)
		v.date = g.date(v.at)
		day += g.n(3, 24)
		sizes := []picSize{{w: tw, h: th, pw: 192, ph: 108}, {w: bw, h: bh}}
		if i == 0 { // 首页的精选:宽一些的大图
			sizes = append(sizes, picSize{w: ww, h: wh, pw: 320, ph: 140})
		}
		pics := s.pictureOf(g, i, photo, kind, tod, sizes...)
		v.thumb, v.big = pics[0], pics[1]
		if i == 0 {
			v.wide = pics[2]
		}
		vids = append(vids, v)
	}

	watch := g.sk.watch
	thumb := func(v vid) string {
		return fmt.Sprintf(`<div class="%s">%s<span class="%s">%s</span></div>`, g.c("thumb"), g.img("", v.thumb, tw, th, true), g.c("dur"), v.dur)
	}
	card := func(v vid) string {
		return fmt.Sprintf(`<a href="%s%s">%s<div class="%s">%s</div><div class="%s %s">%s%s%s</div></a>`,
			watch, v.id, thumb(v), g.c("vt"), esc(v.title), g.c("muted"), g.c("meta"), esc(views(v.views)), dot, v.date)
	}
	gate := g.id(6) // 「会员才能看」那一层的 id(按种子换)
	// 播放按钮:圆的大小、深浅、三角的位置各台差一点(同一段 SVG 各台一字不差的话也能拿去搜)
	box := []int{48, 64, 72, 80, 96, 100}[g.r.IntN(6)] // 画布多大也不一样,里面的坐标跟着缩放
	sc := func(v int) string { return ff(float64(v) * float64(box) / 80) }
	tri := 2 * g.n(14, 16) // 三角的高
	playIcon := fmt.Sprintf(`<svg viewBox="0 0 %d %d"><circle cx="%s" cy="%s" r="%s" fill="rgba(0,0,0,.%d)"/><path d="M%s %sv%sl%s-%sz" fill="#fff"/></svg>`,
		box, box, sc(40), sc(40), sc(g.n(34, 38)), g.n(36, 55), sc(g.n(31, 33)), sc(g.n(24, 26)), sc(tri), sc(g.n(22, 26)), sc(tri/2))
	playLabel := g.say("Play", "Play video", "Watch", "Start", "Play")
	gateText := fmt.Sprintf(g.say("%[1]s to watch this video. Membership is by invitation.", "This video is for members. %[1]s to keep watching.",
		"You need a member account to play this. Please %[2]s first.", "Playback is limited to members. %[1]s, or create an account with an invite.",
		"Members can watch the full recording. %[1]s to continue.", "Please %[2]s to start watching.", "This one is in the members' library. %[1]s to play it."), in, inL)
	closeWord := g.say("Close", "Not now", "Dismiss", "Cancel", "Back", "Maybe later")
	quality := g.say("HD", "4K", "1080p", "HQ", "Auto", "HD")
	fullNote := g.say("Playback is available to signed-in members.", "Members can watch it in full.", "The full recording is in the members' library.", "",
		"Full video for members.", "Available to members in up to 4K.", "Streaming for members only.")
	detLabels := [4]string{g.say("Recorded", "Filmed", "Shot", "Date"), g.say("Length", "Duration", "Runtime", "Running time"), g.say("Quality", "Resolution", "Video", "Format"), g.say("Audio", "Sound", "Audio track", "Sound mix")}
	// 画质、声音:每个站只用其中三四种(原来四种里挑,十二个视频下来每个站四种都有)
	qualities := g.pickN([]string{"4K HDR", "4K", "1080p 60 fps", "4K 60 fps", "1440p", "4K 30 fps", "1080p", "2160p HDR", "UHD", "5K", "1080p HDR", "4K 24 fps"}, g.n(2, 4))
	audios := g.pickN([]string{"Stereo", "Stereo 48 kHz", "Binaural", "5.1 surround", "Ambient stereo", "Stereo 96 kHz", "Mono", "Field recording", "Stereo (AAC)", "Dolby Atmos"}, g.n(2, 3))
	comNoun := g.pick([]string{"comments", "comments", "replies", "responses"}) // 评论在这个站叫什么:标题和下面那句用同一个词
	comHead := g.say(capFirst(comNoun), capFirst(comNoun), "Discussion", "Member "+comNoun, "Conversation")
	com := []linked{{"%[1]d %[2]s" + dot, in, " to read them and join the conversation."}, {"%[1]d %[2]s. ", in, " to see what members are saying."},
		{"There are %[1]d %[2]s on this video. ", in, " to read or reply."}, {"%[1]d %[2]s from members" + dot, in, " to join in."},
		{capFirst(comNoun) + " (%[1]d) are visible to members. ", in, " to read them."}, {"%[1]d %[2]s so far. ", in, " to add yours."}}[g.r.IntN(6)]
	upHead := g.say("Up next", "More videos", "Watch next", "Related", "Next up", "Keep watching", "More like this")
	recFmt := g.pick([]string{"Jan 2006", "January 2006", "2006", "Jan 2006", "01/2006"})
	notes := newVideoNotes(g)
	for i, v := range vids {
		var upn strings.Builder
		for k := 1; k <= 6; k++ {
			u := vids[(i+k)%len(vids)]
			fmt.Fprintf(&upn, `<a href="%s%s">%s<div><b>%s</b><span class="%s">%s%s%s</span></div></a>`,
				watch, u.id, thumb(u), esc(u.title), g.c("muted"), esc(views(u.views)), dot, u.date)
		}
		wp := watch + v.id
		next := "?" + g.sk.nextKey + "=" + wp
		note := notes()
		if fullNote != "" {
			note += " " + fullNote
		}
		player := fmt.Sprintf(`<div class="%s">%s`+
			`<div class="%s" id="%s"><span class="%s"></span><div class="%s"><b>%s</b><span>%s</span>`+
			`<div class="%s"><a class="%s" href="%s%s">%s</a><a class="%s" href="%s%s">%s</a></div><a href="#">%s</a></div></div>`+
			`<a href="#%s" aria-label="%s" data-%s="%s">%s</a><div class="%s"><s></s><span>0:00 / %s</span><i></i><span>%s</span></div></div>`,
			g.c("hero"), g.img("", v.big, bw, bh, false),
			g.c("gate"), gate, g.c("spin"), g.c("msg"), esc(members), esc(gateText),
			g.c("acts"), g.c("btn"), ap.login, next, esc(in), g.c("btn2"), ap.register, next, esc(up), esc(closeWord),
			gate, esc(playLabel), g.sk.playKey, v.id, playIcon, g.c("pbar"), v.dur, esc(quality))
		info := fmt.Sprintf(`<h1>%s</h1><div class="%s"><span>%s</span><div><b>%s</b><span class="%s %s">%s%s%s</span></div><span class="%s">%s</span></div>`+
			`<p>%s</p><div class="%s"><span><b>%s</b>%s</span><span><b>%s</b>%s</span><span><b>%s</b>%s</span><span><b>%s</b>%s</span></div>`,
			esc(v.title), g.c("chan"), esc(s.name[:1]), esc(s.name), g.c("muted"), g.c("meta"), esc(views(v.views)), dot, v.date, g.c("chip"), esc(members),
			esc(note), g.c("det"), esc(detLabels[0]), v.at.AddDate(0, 0, -g.n(3, 40)).Format(recFmt), esc(detLabels[1]), v.dur,
			esc(detLabels[2]), esc(g.pick(qualities)), esc(detLabels[3]), esc(g.pick(audios)))
		comments := fmt.Sprintf(`<h2>%s</h2><p class="%s">%s<a href="%s%s">%s</a>%s</p>`, esc(comHead), g.c("muted"), esc(fmt.Sprintf(com.pre, g.n(3, 140), comNoun)), ap.login, next, esc(com.link), esc(com.post))
		s.add(wp, htmlType, layout(g, s, wp, g.title(v.title, s.name), nav, fmt.Sprintf(`<div class="%s"><div>%s%s%s</div><aside class="%s"><h2>%s</h2>%s</aside></div>`,
			g.c("watch"), player, info, comments, g.c("up"), esc(upHead), upn.String())))
	}

	var grid, cats strings.Builder
	for _, v := range vids {
		grid.WriteString(card(v))
	}
	top := vids[0]
	fmt.Fprintf(&cats, `<span>%s</span>`, esc(g.say("All", "All", "Everything", "All videos", "Latest", "Newest")))
	catList := g.pickN(videoCats, g.n(3, 6))
	g.r.Shuffle(len(catList), func(i, j int) { catList[i], catList[j] = catList[j], catList[i] })
	for _, c := range catList {
		fmt.Fprintf(&cats, `<span>%s</span>`, esc(c))
	}
	s.add("/", htmlType, layout(g, s, "/", s.name, nav, fmt.Sprintf(
		`<a class="%s" href="%s%s">%s<div><span class="%s">%s</span><h2>%s</h2><p>%s%s%s%s%s</p><span class="%s">%s</span></div></a>`+
			`<div class="%s">%s</div><div class="%s">%s</div>`,
		g.c("lead2"), watch, top.id, g.img("", top.wide, ww, wh, false), g.c("kick"), esc(g.say("New this week", "Featured", "Just added", "Staff pick", "Editor's choice", "Latest upload", "This week", "Now showing", "Fresh in the library")),
		esc(top.title), top.dur, dot, esc(views(top.views)), dot, esc(members), g.c("btn"), esc(g.say("Watch now", "Play", "Watch", "Start watching", "Play video", "Watch the film")),
		g.c("cats"), cats.String(), g.c("grid"), grid.String())))

	// ---- 关于 ----
	aboutHead := fill(g.say("About %s", "About", "What is %s?", "About us", "Hello from %s", "The idea behind %s"), s.name)
	aboutP1 := fmt.Sprintf(g.say(
		"%s hosts long recordings for a small community. Videos stream to members through our apps and on this site after signing in; the library above is open to everyone.",
		"%s is a small, member-supported video library. Anyone can browse the catalogue; playback is for members.",
		"We started %s to share long, quiet recordings without ads or autoplay. The catalogue is public, the films are for members.",
		"%s is run by a handful of people who like slow films. You can look around freely; watching needs a member account.",
		"At %s we publish unhurried films for a private audience. Titles and previews are public, the full videos are not.",
		"%s keeps a growing archive of films and sessions. Browsing is open, streaming is reserved for members."), s.name)
	join := []struct{ pre, reg, mid, login, post string }{
		{"Membership is by invitation. If a member has sent you an invite code, you can ", "create your account", "; members can ", inL + " here", "."},
		{"We are invite-only for now. Got a code? ", "Join here", ". Already a member? ", in, "."},
		{"New members join through an invitation from an existing member. ", "Register with your code", " or ", inL, " if you already have an account."},
		{"To keep things small, sign-ups need an invite. ", "Use your invite", ", or ", inL + " to your account", "."},
		{"Accounts are created with a code from a current member: ", "sign up", ". Returning members: ", inL, "."},
		{"Have an invitation? ", "Set up your account", ". Otherwise, members can ", inL + " as usual", "."}}[g.r.IntN(6)]
	s.add("/about/", htmlType, layout(g, s, "/about/", g.title(aboutNav, s.name), nav, fmt.Sprintf(
		`<h1>%s</h1><p class="%s">%s</p><p>%s</p><p>%s<a href="%s">%s</a>%s<a href="%s">%s</a>%s</p><p>%s</p>`,
		esc(aboutHead), g.c("sub"), esc(capFirst(what)+"."), esc(aboutP1),
		esc(join.pre), ap.register, esc(join.reg), esc(join.mid), ap.login, esc(join.login), esc(join.post),
		esc(g.say("New videos are added every week.", "We add new films most weeks.", "There is something new roughly every week.", "The library grows a little every month.",
			"New uploads appear on Fridays.", "We publish a few new recordings each month.", "Fresh recordings land here regularly.")))))

	// ---- 登录、注册、找回密码 ----
	authPage := func(path, title, sub, api, fields, button, alt string) func(string, bool) []byte {
		page := layout(g, s, path, g.title(title, s.name), nav, fmt.Sprintf(
			`<div class="%s"><div class="%s"><h1>%s</h1><p class="%s">%s</p><form method="post" action="%s" data-%s="%s" novalidate>%s%s<button type="submit">%s</button></form><p class="%s">%s</p></div></div>`,
			g.c("auth"), g.c("card"), esc(title), g.c("sub"), esc(sub), s.apiPre+api, g.sk.formKey, api, alertSlot, fields, esc(button), g.c("alt"), alt))
		s.add(path, htmlType, []byte(strings.Replace(string(page), alertSlot, emptyAlert(g), 1)))
		return s.formPage(g, page)
	}
	a := func(href, text string) string { return `<a href="` + href + `">` + esc(text) + `</a>` }
	email := field(g, "email", mailLabel, "email", "email", "", "")
	forgot := g.say("Forgot password?", "Forgot your password?", "Reset password", "Can't "+inL+"?", "Forgot it?", "Need help?")
	loginAlt := []linked{{"New here? Membership is by invitation. ", "Create an account", ""}, {"No account yet? ", "Join with an " + codeNoun, ""}, {"Have an invite? ", "Create your account", ""},
		{"Not a member? ", up, ""}, {"Got an " + codeNoun + "? ", "Sign up here", ""}, {"First time here? ", "Register with your invite", ""}}[g.r.IntN(6)]
	loginForm := authPage(ap.login, g.say(in, in, "Welcome back", "Member "+inL, in+" to your account", "Members"),
		fill(g.say("Welcome back to %s.", "Good to see you again.", "Enter your details to continue.", "Use the email you joined with.", "%s members "+inL+" here.", in+" to keep watching.", "Pick up where you left off."), s.name),
		ap.apiLogin, email+field(g, "password", passLabel, "password", "current-password", "", a(ap.reset, forgot)),
		in, esc(loginAlt.pre)+a(ap.register, loginAlt.link))
	regAlt := []linked{{"Already a member? ", in, ""}, {"Have an account? ", in, ""}, {"Already registered? ", in + " instead", ""}, {"Been here before? ", in, ""}, {"Already joined? ", in, ""}}[g.r.IntN(5)]
	regForm := authPage(ap.register, fill(g.say("Create your account", "Join %s", "Sign up", "Register", "Become a member", "New member", "Create an account"), s.name),
		g.say("Membership is by invitation. Enter the code a member sent you.", "You need an "+codeNoun+" from an existing member to join.", "Have your "+codeNoun+" ready.",
			"Registration is open to invited guests.", "An "+codeNoun+" is required to create an account.", "We're invite-only for now. Enter your code below.", "Joining takes a minute if you have an "+codeNoun+"."),
		ap.apiRegister, email+field(g, "password", passLabel, "password", "new-password", g.say("At least 8 characters.", "8 characters or more.", "Use 8 or more characters.", "Minimum 8 characters.", "Must be at least 8 characters long.", "Eight characters minimum."), "")+
			field(g, inviteKey, capFirst(codeNoun), "text", "off", g.say("Invite codes are sent by existing members and can be used once.", "Ask the member who invited you for a code.", "Each code works once.",
				"You'll find it in your invitation email.", "Codes are single-use and tied to the member who shared them.", "No code? Ask a member to invite you.", "It looks like XXXX-XXXX."), ""),
		up, esc(regAlt.pre)+a(ap.login, regAlt.link))
	resetForm := authPage(ap.reset, g.say("Reset your password", "Forgot your password?", "Password reset", "Recover your account", "Trouble signing in?", "Choose a new password", "Reset password"),
		g.say("Enter the email you signed up with and we'll send you a link to choose a new password.", "Tell us your email and we'll send reset instructions.", "We'll email you a link to set a new password.",
			"Enter your account email to get a reset link.", "Type the email on your account. If we find it, you'll get a message with next steps.", "No problem. Enter your email and we'll help you get back in.",
			"It happens. We'll send a link to the address on your account."),
		ap.apiReset, email, g.say("Send reset link", "Email me a link", "Send instructions", "Continue", "Reset password", "Send link", "Send email"),
		a(ap.login, g.say("Back to "+inL, "Return to "+inL, "Remembered it? "+in, "Cancel", "Go back", "Never mind, "+inL)))

	loginErr := g.say("Incorrect email or password.", "The email or password you entered is incorrect.", "Wrong email or password. Please try again.", "Invalid email or password.",
		"We couldn't "+inL+" with that email and password.", "Email and password don't match.", "That email and password combination is not right.")
	inviteErr := fmt.Sprintf(g.say("This %[1]s is not valid or has already been used.", "That %[1]s isn't valid. Codes come from existing members.", "Invalid or expired %[1]s.", "We don't recognise that %[1]s.",
		"That %[1]s has already been used.", "This %[1]s can't be used. Ask the member who invited you for a new one."), codeNoun)
	resetOK := g.say("If an account exists for that email, we've sent a link to reset your password.", "Check your email: if an account exists, a reset link is on its way.",
		"Thanks. If that address is registered, you'll get an email shortly.", "If we know that address, a reset link is on its way.", "Done. Look for an email from us in the next few minutes.",
		"We've sent reset instructions if the address matches an account.")
	badFields := g.say("Please check the highlighted fields.", "Some fields need your attention.", "Please correct the errors below.", "Check the form and try again.", "A few things need fixing.", "Please review the marked fields.")
	badEmail := g.say("Enter a valid email address.", "Please enter a valid email.", "That email address doesn't look right.", "Check your email address.", "A valid email is required.")
	noPass := g.say("Enter your password.", "Password is required.", "Please enter your password.", "Type your password.", "Don't forget your password.")
	shortPass := g.say("Use at least 8 characters.", "Password must be at least 8 characters.", "That password is too short.", "Choose a longer password (8+ characters).", "At least 8 characters, please.")
	noCode := fmt.Sprintf(g.say("Enter your %s.", "An %s is required.", "Please enter your %s.", "You need an %s to join.", "Add your %s."), codeNoun)
	checkEmail := func(in map[string]string, f map[string]string) {
		if !emailRe.MatchString(strings.TrimSpace(in["email"])) {
			f["email"] = badEmail
		}
	}
	s.postAPI(s.apiPre+ap.apiLogin, true, func(in map[string]string) apiReply {
		f := map[string]string{}
		checkEmail(in, f)
		if in["password"] == "" {
			f["password"] = noPass
		}
		if len(f) > 0 {
			return apiReply{code: http.StatusUnprocessableEntity, err: "validation_failed", msg: badFields, fields: f}
		}
		return apiReply{code: http.StatusUnauthorized, err: "invalid_credentials", msg: loginErr}
	}, loginForm)
	s.postAPI(s.apiPre+ap.apiRegister, true, func(in map[string]string) apiReply {
		f := map[string]string{}
		checkEmail(in, f)
		if len(in["password"]) < 8 {
			f["password"] = shortPass
		}
		if strings.TrimSpace(in[inviteKey]) == "" {
			f[inviteKey] = noCode
		}
		if len(f) > 0 {
			return apiReply{code: http.StatusUnprocessableEntity, err: "validation_failed", msg: badFields, fields: f}
		}
		return apiReply{code: http.StatusForbidden, err: "invalid_invite", msg: inviteErr, fields: map[string]string{inviteKey: inviteErr}}
	}, regForm)
	s.postAPI(s.apiPre+ap.apiReset, false, func(in map[string]string) apiReply {
		f := map[string]string{}
		if checkEmail(in, f); len(f) > 0 {
			return apiReply{code: http.StatusUnprocessableEntity, err: "validation_failed", msg: badFields, fields: f}
		}
		return apiReply{code: http.StatusAccepted, ok: true, msg: resetOK}
	}, resetForm)

	// ---- 只读的接口:当前用户(没登录)、视频列表 / 详情;播放一律要先登录 ----
	s.getJSON(s.apiPre+"me", http.StatusUnauthorized, s.errBody(http.StatusUnauthorized, "unauthorized", s.apiText.auth, nil))
	k := g.jsonKey
	playMsg := g.say(in+" to watch this video.", "Members only.", "Please "+inL+" to play this video.", "This video requires a member account.", "Authentication required to start playback.")
	var items []map[string]any
	for _, v := range vids {
		it := map[string]any{"id": v.id, "title": v.title, k("duration_seconds"): v.secs, k("published_at"): v.at.UTC().Format(time.RFC3339),
			"views": v.views, k("thumbnail_url"): v.thumb, k("members_only"): true}
		items = append(items, it)
		s.getJSON(s.apiPre+"videos/"+v.id, http.StatusOK, it)
		s.postAPI(s.apiPre+"videos/"+v.id+"/play", false, func(map[string]string) apiReply {
			return apiReply{code: http.StatusUnauthorized, err: "auth_required", msg: playMsg}
		}, nil)
	}
	s.getJSON(s.apiPre+"videos", http.StatusOK, map[string]any{"data": items, "page": 1, k("per_page"): 24, "total": len(items)})
	s.setNotFound(layout(g, s, "", nfTitle(g, s.name), nav, notFoundBody(g)))
}

// viewCount 播放次数(大多几千,少数几万、十几万)
func viewCount(g *gen) int {
	switch n := g.n(3, 280); {
	case n < 10:
		return n*100 + g.n(0, 99)
	case n < 100:
		return n*100 + g.n(0, 99)*10
	default:
		return n*500 + g.n(0, 499)
	}
}

// fmtViews 播放次数的写法:1234 → 1.2K,123456 → 123K
func fmtViews(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 100000:
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	default:
		return fmt.Sprintf("%dK", n/1000)
	}
}

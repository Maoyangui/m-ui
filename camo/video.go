package camo

// 视频站(会员制片库,用户 11-19 精修):首页一张大的精选,下面是片库;播放页左边播放器、右边「接下来」;
// 点播放先转一会儿圈,再说只给会员看,给「登录」「注册」两个按钮。顶栏有「登录」。
// 登录、注册(要邀请码 —— 实际没有,怎么填都不通过,只为把注册挡住)、找回密码三页,表单经接口提交(api.go):
// 开着脚本时是页面里的脚本发 JSON,没开时表单直接提交、回一页带提示的同一页。还有当前用户(一律没登录)、
// 视频列表 / 详情、播放(一律要先登录)几个接口。封面有图包照片就用照片(media.go),没有画插图。

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
		{"/account/login", "/account/register", "/account/recover"}, {"/sign-in", "/join", "/reset-password"}}
	apis := [][3]string{{"auth/login", "auth/register", "auth/password/forgot"}, {"session", "users", "password/reset"},
		{"login", "signup", "password-reset"}, {"auth/signin", "auth/signup", "auth/recover"}}
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

func genVideo(s *Site, g *gen) {
	s.name = g.pick(words) + " " + g.pick([]string{"Video", "Stream", "Screen", "Reels"})
	s.desc = s.name + g.pick([]string{" is a members-only library of travel films, city walks and studio sessions.",
		": long recordings, travel films and studio sessions for our members.", " streams quiet travel films and studio recordings to members."})
	s.logo = `<span class="` + g.c("logo") + ` ` + g.c("play") + `"></span>`
	s.foot = `<span>Streaming for members in our apps</span>`
	s.apiPre = g.sk.api
	ap := pickAuthPaths(g)
	inviteKey := g.pick([]string{"invite", "invite_code", "inviteCode", "code"})
	s.cta = fmt.Sprintf(`<a class="%s" href="%s">Sign in</a>`, g.c("cta"), ap.login)
	s.addScript(g, scriptParts{forms: true, me: "me", play: true, formKey: g.sk.formKey, playKey: g.sk.playKey})
	nav := [][2]string{{"/", "Library"}, {"/about/", "About"}}

	type vid struct {
		id, title, dur, date, thumb, big, wide string
		secs, views                            int
		at                                     time.Time
	}
	var vids []vid
	used := map[string]bool{}
	day := g.n(1, 5)
	for i := 0; i < 12; i++ {
		ti, kind, tod, photo := s.videoPick(g, i, used)
		v := vid{id: g.id(11), title: ti, secs: g.n(2, 48)*60 + g.n(0, 59), views: viewCount(g)}
		v.dur = fmt.Sprintf("%d:%02d", v.secs/60, v.secs%60)
		v.at = g.base.Add(-time.Duration(day)*24*time.Hour + time.Duration(g.n(7, 21))*time.Hour)
		v.date = v.at.Format("Jan 2, 2006")
		day += g.n(3, 24)
		sizes := []picSize{{w: 480, h: 270, pw: 192, ph: 108}, {w: 960, h: 540}}
		if i == 0 { // 首页的精选:宽一些的大图
			sizes = append(sizes, picSize{w: 1280, h: 560, pw: 320, ph: 140})
		}
		pics := s.pictureOf(g, i, photo, kind, tod, sizes...)
		v.thumb, v.big = pics[0], pics[1]
		if i == 0 {
			v.wide = pics[2]
		}
		vids = append(vids, v)
	}

	thumb := func(v vid) string {
		return fmt.Sprintf(`<div class="%s"><img src="%s" alt="" width="480" height="270" loading="lazy"><span class="%s">%s</span></div>`, g.c("thumb"), v.thumb, g.c("dur"), v.dur)
	}
	card := func(v vid) string {
		return fmt.Sprintf(`<a href="/watch/%s">%s<div class="%s">%s</div><div class="%s" style="font-size:13px">%s views · %s</div></a>`,
			v.id, thumb(v), g.c("vt"), esc(v.title), g.c("muted"), fmtViews(v.views), v.date)
	}
	gate := g.id(6) // 「会员才能看」那一层的 id(按种子换)
	playIcon := `<svg viewBox="0 0 80 80"><circle cx="40" cy="40" r="38" fill="rgba(0,0,0,.45)"/><path d="M32 25v30l24-15z" fill="#fff"/></svg>`
	for i, v := range vids {
		var up strings.Builder
		for k := 1; k <= 6; k++ {
			u := vids[(i+k)%len(vids)]
			fmt.Fprintf(&up, `<a href="/watch/%s">%s<div><b>%s</b><span class="%s">%s views · %s</span></div></a>`,
				u.id, thumb(u), esc(u.title), g.c("muted"), fmtViews(u.views), u.date)
		}
		wp := "/watch/" + v.id
		next := "?next=" + wp
		s.add(wp, htmlType, layout(g, s, wp, v.title+" - "+s.name, nav, fmt.Sprintf(
			`<div class="%s"><div><div class="%s"><img src="%s" alt="" width="960" height="540">`+
				`<div class="%s" id="%s"><span class="%s"></span><div class="%s"><b>Members only</b><span>Sign in to watch this video. Membership is by invitation.</span>`+
				`<div class="%s"><a class="%s" href="%s%s">Sign in</a><a class="%s" href="%s%s">Create account</a></div><a href="#">Close</a></div></div>`+
				`<a href="#%s" aria-label="Play" data-%s="%s">%s</a><div class="%s"><s></s><span>0:00 / %s</span><i></i><span>HD</span></div></div>`+
				`<h1 style="margin-top:18px">%s</h1><div class="%s"><span>%s</span><div><b>%s</b><span class="%s" style="font-size:13px">%s views · %s</span></div><span class="%s" style="margin:0 0 0 auto">Members only</span></div>`+
				`<p>%s Playback is available to signed-in members.</p>`+
				`<div class="%s"><span><b>Recorded</b>%s</span><span><b>Length</b>%s</span><span><b>Quality</b>%s</span><span><b>Audio</b>%s</span></div>`+
				`<h2>Comments</h2><p class="%s">%d comments · <a href="%s%s">Sign in</a> to read them and join the conversation.</p></div><aside class="%s"><h2>Up next</h2>%s</aside></div>`,
			g.c("watch"), g.c("hero"), v.big,
			g.c("gate"), gate, g.c("spin"), g.c("msg"),
			g.c("acts"), g.c("btn"), ap.login, next, g.c("btn2"), ap.register, next,
			gate, g.sk.playKey, v.id, playIcon, g.c("pbar"), v.dur,
			esc(v.title), g.c("chan"), esc(s.name[:1]), esc(s.name), g.c("muted"), fmtViews(v.views), v.date, g.c("chip"),
			esc(videoNote(g)),
			g.c("det"), v.at.AddDate(0, 0, -g.n(3, 40)).Format("Jan 2006"), v.dur, g.pick([]string{"4K · HDR", "4K", "1080p · 60 fps", "4K · 60 fps"}), g.pick([]string{"Stereo", "Stereo · 48 kHz", "Binaural", "5.1 surround"}),
			g.c("muted"), g.n(3, 140), ap.login, next,
			g.c("up"), up.String())))
	}

	var grid strings.Builder
	for _, v := range vids {
		grid.WriteString(card(v))
	}
	top := vids[0]
	cats := `<span>All</span><span>Travel</span><span>City</span><span>Nature</span><span>Studio</span>`
	s.add("/", htmlType, layout(g, s, "/", s.name, nav, fmt.Sprintf(
		`<a class="%s" href="/watch/%s"><img src="%s" alt="" width="1280" height="560"><div><span class="%s">%s</span><h2>%s</h2><p>%s · %s views · Members only</p><span class="%s">Watch now</span></div></a>`+
			`<div class="%s">%s</div><div class="%s">%s</div>`,
		g.c("lead2"), top.id, top.wide, g.c("kick"), g.pick([]string{"New this week", "Featured", "Just added", "Staff pick"}), esc(top.title), top.dur, fmtViews(top.views), g.c("btn"),
		g.c("cats"), cats, g.c("grid"), grid.String())))
	s.add("/about/", htmlType, layout(g, s, "/about/", "About - "+s.name, nav, fmt.Sprintf(
		`<h1>About %s</h1><p class="%s">Recordings, travel films and studio sessions.</p><p>%s hosts long recordings for a small community. Videos stream to members through our apps and on this site after signing in; the library above is open to everyone.</p>`+
			`<p>Membership is by invitation. If a member has sent you an invite code, you can <a href="%s">create your account</a>; members can <a href="%s">sign in here</a>.</p><p>New videos are added every week.</p>`,
		esc(s.name), g.c("sub"), esc(s.name), ap.register, ap.login)))

	// ---- 登录、注册、找回密码 ----
	authPage := func(path, title, sub, api, fields, button, alt string) func(string, bool) []byte {
		page := layout(g, s, path, title+" - "+s.name, nav, fmt.Sprintf(
			`<div class="%s"><div class="%s"><h1>%s</h1><p class="%s">%s</p><form method="post" action="%s" data-%s="%s" novalidate>%s%s<button type="submit">%s</button></form><p class="%s">%s</p></div></div>`,
			g.c("auth"), g.c("card"), esc(title), g.c("sub"), sub, s.apiPre+api, g.sk.formKey, api, alertSlot, fields, esc(button), g.c("alt"), alt))
		s.add(path, htmlType, []byte(strings.Replace(string(page), alertSlot, emptyAlert(g), 1)))
		return s.formPage(g, page)
	}
	email := field(g, "email", "Email", "email", "email", "", "")
	loginForm := authPage(ap.login, "Sign in", "Welcome back to "+esc(s.name)+".", ap.apiLogin,
		email+field(g, "password", "Password", "password", "current-password", "", `<a href="`+ap.reset+`">Forgot password?</a>`),
		"Sign in", `New here? Membership is by invitation. <a href="`+ap.register+`">Create an account</a>`)
	regForm := authPage(ap.register, "Create your account", "Membership is by invitation. Enter the code a member sent you.", ap.apiRegister,
		email+field(g, "password", "Password", "password", "new-password", "At least 8 characters.", "")+
			field(g, inviteKey, "Invite code", "text", "off", "Invite codes are sent by existing members and can be used once.", ""),
		"Create account", `Already a member? <a href="`+ap.login+`">Sign in</a>`)
	resetForm := authPage(ap.reset, "Reset your password", "Enter the email you signed up with and we'll send you a link to choose a new password.", ap.apiReset,
		email, "Send reset link", `<a href="`+ap.login+`">Back to sign in</a>`)

	loginErr := g.pick([]string{"Incorrect email or password.", "The email or password you entered is incorrect.", "Wrong email or password. Please try again."})
	inviteErr := g.pick([]string{"This invite code is not valid or has already been used.", "That invite code isn't valid. Invite codes come from existing members.", "Invalid or expired invite code."})
	resetOK := g.pick([]string{"If an account exists for that email, we've sent a link to reset your password.", "Check your email: if an account exists, a reset link is on its way."})
	badFields := g.pick([]string{"Please check the highlighted fields.", "Some fields need your attention.", "Please correct the errors below."})
	checkEmail := func(in map[string]string, f map[string]string) {
		if !emailRe.MatchString(strings.TrimSpace(in["email"])) {
			f["email"] = "Enter a valid email address."
		}
	}
	s.postAPI(s.apiPre+ap.apiLogin, true, func(in map[string]string) apiReply {
		f := map[string]string{}
		checkEmail(in, f)
		if in["password"] == "" {
			f["password"] = "Enter your password."
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
			f["password"] = "Use at least 8 characters."
		}
		if strings.TrimSpace(in[inviteKey]) == "" {
			f[inviteKey] = "Enter your invite code."
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
	s.getJSON(s.apiPre+"me", http.StatusUnauthorized, s.errBody(http.StatusUnauthorized, "unauthorized", "Authentication required.", nil))
	k := g.jsonKey
	var items []map[string]any
	for _, v := range vids {
		it := map[string]any{"id": v.id, "title": v.title, k("duration_seconds"): v.secs, k("published_at"): v.at.UTC().Format(time.RFC3339),
			"views": v.views, k("thumbnail_url"): v.thumb, k("members_only"): true}
		items = append(items, it)
		s.getJSON(s.apiPre+"videos/"+v.id, http.StatusOK, it)
		s.postAPI(s.apiPre+"videos/"+v.id+"/play", false, func(map[string]string) apiReply {
			return apiReply{code: http.StatusUnauthorized, err: "auth_required", msg: "Sign in to watch this video."}
		}, nil)
	}
	s.getJSON(s.apiPre+"videos", http.StatusOK, map[string]any{"data": items, "page": 1, k("per_page"): 24, "total": len(items)})
	s.setNotFound(layout(g, s, "", "Not found - "+s.name, nav, notFoundBody(g)))
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

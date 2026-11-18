package camo

// 伪装站的「后端」(用户 11-19:视频站播放要登录,那就要有注册登录,注册必须填邀请码 —— 实际没有,只为限制注册;
// 「做一些假的没用的后端 api post 等,让别人看代码也像真站」)。
//
// 视频站:登录、注册(要邀请码,怎么填都不通过)、找回密码、当前用户(一律没登录)、视频列表 / 详情、播放(一律要先登录);
// 软件下载:检查更新(最新版本和各平台的文件)、邮件订阅;个人博客:邮件订阅;镜像站:同步状态。
// 页面里有一小段脚本(script)调用它们:看源码、看浏览器里的网络请求,都像一个真的站。接口前缀、字段名、报错的写法
// (几种常见框架的样子)、脚本里的名字都按种子换。
//
// 只回固定的内容:提交上来的东西不存、不记日志、不回显;请求体最多读 16 KB。登录、注册像真站那样慢一点(算密码哈希的工夫),
// 同时最多 16 个请求在等,再多的立即回,拖不住连接。不发 Cookie:伪装站和面板、订阅可能在同一个域名上,
// Cookie 只认域名不认端口,不能去碰它们的。

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const jsonType = "application/json; charset=utf-8"
const jsType = "text/javascript; charset=utf-8"
const jpegType = "image/jpeg"

// apiMaxBody 接口读的请求体上限
const apiMaxBody = 16 << 10

// apiRoute 一个接口
type apiRoute struct {
	allow string                              // 允许的方法(Allow 头的写法)
	code  int                                 // GET:状态码
	body  []byte                              // GET:JSON
	post  func(in map[string]string) apiReply // POST:按提交的字段回
	slow  bool                                // 慢一点(登录、注册)
	form  func(msg string, ok bool) []byte    // 表单直接提交(没开脚本)时回的页面;nil 时一律回 JSON
}

// apiReply POST 的结果
type apiReply struct {
	code   int
	ok     bool              // 成功(找回密码、订阅)
	msg    string            // 给人看的一句
	err    string            // 错误代码(snake_case)
	fields map[string]string // 哪几个字段不对
}

// getJSON 加一个只读接口(GET / HEAD)
func (s *Site) getJSON(path string, code int, v any) {
	b, _ := json.Marshal(v)
	s.api[path] = &apiRoute{allow: "GET, HEAD", code: code, body: b}
}

// postAPI 加一个提交接口
func (s *Site) postAPI(path string, slow bool, fn func(in map[string]string) apiReply, form func(msg string, ok bool) []byte) {
	s.api[path] = &apiRoute{allow: "POST", post: fn, slow: slow, form: form}
}

func (s *Site) serveAPI(w http.ResponseWriter, r *http.Request, p string) bool {
	rt := s.api[p]
	if rt == nil {
		if s.apiPre == "" || !strings.HasPrefix(p, s.apiPre) {
			return false
		}
		s.writeAPI(w, r, http.StatusNotFound, s.errJSON(http.StatusNotFound, "not_found", "Not found.", nil))
		return true
	}
	if !strings.Contains(", "+rt.allow+",", ", "+r.Method+",") {
		w.Header().Set("Allow", rt.allow)
		s.writeAPI(w, r, http.StatusMethodNotAllowed, s.errJSON(http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.", nil))
		return true
	}
	if rt.post == nil {
		s.writeAPI(w, r, rt.code, rt.body)
		return true
	}
	in, isJSON, err := readFields(w, r)
	if err != nil {
		code := http.StatusBadRequest
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			code = http.StatusRequestEntityTooLarge
		}
		s.writeAPI(w, r, code, s.errJSON(code, "bad_request", "The request body could not be read.", nil))
		return true
	}
	rep := rt.post(in)
	if rt.slow {
		pause(r)
	}
	if !isJSON && rt.form != nil {
		// 没开脚本的提交回的页面上不会标出哪个字段不对:格式没过时把各字段的提示直接写出来(「请检查标出来的字段」在这一页上对不上,自审 G2)
		msg := rep.msg
		if !rep.ok && len(rep.fields) > 0 {
			var parts []string
			for _, k := range fieldOrder(rep.fields) {
				if f := rep.fields[k]; !strings.Contains(strings.Join(parts, " "), f) {
					parts = append(parts, f)
				}
			}
			msg = strings.Join(parts, " ")
		}
		body := rt.form(msg, rep.ok)
		h := w.Header()
		h.Set("Content-Type", htmlType)
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(rep.code)
		_, _ = w.Write(body)
		return true
	}
	var v any
	if rep.ok {
		v = map[string]any{"ok": true, "message": rep.msg}
	} else {
		v = s.errBody(rep.code, rep.err, rep.msg, rep.fields)
	}
	b, _ := json.Marshal(v)
	s.writeAPI(w, r, rep.code, b)
	return true
}

func (s *Site) writeAPI(w http.ResponseWriter, r *http.Request, code int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", jsonType)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// errJSON / errBody 报错:按这个站的「框架」写(errStyle,种子定),几种常见的写法
func (s *Site) errJSON(code int, key, msg string, fields map[string]string) []byte {
	b, _ := json.Marshal(s.errBody(code, key, msg, fields))
	return b
}

func (s *Site) errBody(code int, key, msg string, fields map[string]string) any {
	switch s.errStyle {
	case 1: // {"detail": …}:校验没过时是逐个字段的列表,别的是一句话
		if code == http.StatusUnprocessableEntity && len(fields) > 0 {
			var list []map[string]any
			for _, k := range fieldOrder(fields) {
				list = append(list, map[string]any{"loc": []string{"body", k}, "msg": fields[k], "type": "value_error"})
			}
			return map[string]any{"detail": list}
		}
		return map[string]any{"detail": msg}
	case 2: // {"statusCode": …, "error": …, "message": …}
		m := map[string]any{"statusCode": code, "error": http.StatusText(code), "message": msg}
		if len(fields) > 0 {
			m["errors"] = fields
		}
		return m
	}
	m := map[string]any{"error": key, "message": msg}
	if len(fields) > 0 {
		m["fields"] = fields
	}
	return m
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ { // 字段只有两三个
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// fieldOrder 报错里各字段的先后:按表单从上到下的顺序(邮箱、密码在前,邀请码之类在后),同一档按名字。
// 没开脚本时那一句提示、FastAPI 那种逐字段的列表都这样排;以前只按名字排,注册页会先说邀请码、再说密码(自审第五遍 G9,实机看到的)。
func fieldOrder(m map[string]string) []string {
	out := sortedKeys(m)
	rank := func(k string) int {
		switch k {
		case "email":
			return 0
		case "password":
			return 1
		}
		return 2
	}
	for i := 1; i < len(out); i++ { // 插入排序是稳定的:同一档保持按名字的先后
		for j := i; j > 0 && rank(out[j]) < rank(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// readFields 读提交的字段:JSON(脚本提交)或表单(没开脚本)。只取字符串,别的类型一律当没填。
func readFields(w http.ResponseWriter, r *http.Request) (map[string]string, bool, error) {
	r.Body = http.MaxBytesReader(w, r.Body, apiMaxBody)
	in := map[string]string{}
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		var m map[string]any
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			return nil, true, err
		}
		for k, v := range m {
			if s, ok := v.(string); ok {
				in[k] = s
			}
		}
		return in, true, nil
	}
	if err := r.ParseForm(); err != nil {
		return nil, false, err
	}
	for k, v := range r.PostForm {
		if len(v) > 0 {
			in[k] = v[0]
		}
	}
	return in, false, nil
}

var slowSlots = make(chan struct{}, 16)

// pause 像算密码哈希那样等一小会儿(180–520 毫秒);同时在等的满了就不等
func pause(r *http.Request) {
	select {
	case slowSlots <- struct{}{}:
		defer func() { <-slowSlots }()
		t := time.NewTimer(time.Duration(180+rand.IntN(340)) * time.Millisecond)
		defer t.Stop()
		select {
		case <-t.C:
		case <-r.Context().Done():
		}
	default:
	}
}

var emailRe = regexp.MustCompile(`^[^@\s"<>]{1,64}@[A-Za-z0-9.-]{1,190}\.[A-Za-z]{2,24}$`)

// ---- 表单页 ----

// alertSlot 表单页里放提示的位置:页面生成时留一个记号,没开脚本时表单直接提交过来,把提示填进去再回这一页
const alertSlot = "\x00alert\x00"

// formPage 把带记号的页面拆成两半,回一个往中间填提示的函数(提示只会是固定的几句,不回显提交的内容)
func (s *Site) formPage(g *gen, body []byte) func(msg string, ok bool) []byte {
	head, tail, found := strings.Cut(string(body), alertSlot)
	if !found {
		return nil
	}
	return func(msg string, ok bool) []byte {
		cls := g.c("alert")
		if ok {
			cls += " " + g.c("okay")
		}
		return []byte(head + `<div class="` + cls + `" role="alert">` + esc(msg) + `</div>` + tail)
	}
}

// emptyAlert 页面正常打开时那个位置:一个藏着的提示框(脚本往里填)
func emptyAlert(g *gen) string {
	return `<div class="` + g.c("alert") + `" role="alert" hidden></div>`
}

// field 表单的一项
func field(g *gen, name, label, typ, auto, hint, extra string) string {
	h := ""
	if hint != "" {
		h = `<small>` + esc(hint) + `</small>`
	}
	return fmt.Sprintf(`<label class="%s"><span>%s%s</span><input name="%s" type="%s" autocomplete="%s" required>%s</label>`,
		g.c("field"), esc(label), extra, name, typ, auto, h)
}

// ---- 脚本 ----

// jsNames 脚本里的几个名字(按种子打乱,像压缩工具出的单字母名)
func jsNames(g *gen, n int) []string {
	pool := strings.Split("a b c d e f h i j k l m n o p r s u v w x y z A B C D E F G H I J K L M N O P Q R S T U V W X Y Z", " ")
	g.r.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	return pool[:n]
}

// script 页面脚本:取接口的小函数,加上这个站要的几段(表单提交、查登录状态、点播放、按系统挑下载)
type scriptParts struct {
	forms   bool   // 表单用接口提交
	me      string // 打开页面时查一次当前用户的接口(相对前缀)
	play    bool   // 点播放时报给接口
	os      bool   // 按访客的系统把对应的下载放到第一个
	latest  string // 打开页面时查一次最新版本的接口(相对前缀)
	formKey string // 表单上写接口地址的 data- 属性
	playKey string // 播放按钮上写视频 id 的 data- 属性
	osKey   string // 下载卡片上写系统的 data- 属性
}

func (s *Site) addScript(g *gen, p scriptParts) {
	v := jsNames(g, 13)
	A, Q, R, f, m, b, d, x, k, o, c, e, y := v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7], v[8], v[9], v[10], v[11], v[12]
	var js strings.Builder
	fmt.Fprintf(&js, `(()=>{"use strict";const %s=%q,%s=(%s,%s=document)=>%s.querySelector(%s),%s=(%s,%s={})=>fetch(%s+%s,{credentials:"same-origin",...%s,headers:{Accept:"application/json",...%s.headers}}).then(async %s=>({ok:%s.ok,status:%s.status,data:await %s.json().catch(()=>({}))}));`,
		A, s.apiPre, Q, k, c, c, k, R, k, o, A, k, o, o, e, e, e, e)
	if p.me != "" {
		fmt.Fprintf(&js, `%s(%q).then(%s=>{%s.ok&&document.documentElement.classList.add(%q)}).catch(()=>{});`, R, p.me, e, e, g.c("authed"))
	}
	if p.forms {
		busy, bad, alert, okay := g.c("busy2"), g.c("bad"), g.c("alert"), g.c("okay")
		fmt.Fprintf(&js, `document.querySelectorAll("form[data-%s]").forEach(%s=>{const %s=%s("[role=alert]",%s),%s=%s("button[type=submit]",%s);%s.addEventListener("submit",async %s=>{if(%s.preventDefault(),%s.disabled)return;%s.disabled=!0,%s.classList.add(%q),%s.hidden=!0,%s.querySelectorAll(".%s").forEach(%s=>%s.classList.remove(%q));const %s=Object.fromEntries(new FormData(%s));try{const %s=await %s(%s.dataset.%s,{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(%s)}),%s=%s.data||{};const %s=Array.isArray(%s.detail)?%s.detail:[];%s.className=%s.ok?%q:%q,%s.textContent=%s.message||(typeof %s.detail=="string"?%s.detail:%s[0]&&%s[0].msg||(%s.ok?"Done.":"Please check the form and try again.")),%s.hidden=!1;const %s=%s.fields||%s.errors||Object.fromEntries(%s.map(%s=>[%s.loc&&%s.loc[1],%s.msg]));for(const %s in %s){const %s=%s.elements[%s];%s&&%s.classList&&%s.classList.add(%q)}%s.ok&&%s.reset()}catch{%s.className=%q,%s.textContent="Network error. Please try again.",%s.hidden=!1}finally{%s.disabled=!1,%s.classList.remove(%q)}})});`,
			p.formKey, f, m, Q, f, b, Q, f, f, e, e, b, b, b, busy, m, f, bad, x, x, bad, d, f, o, R, f, jsCamel(p.formKey), d, x, o,
			y, x, x, m, o, alert+" "+okay, alert, m, x, x, x, y, y, o, m,
			c, x, x, y, e, e, e, e, k, c, e, f, k, e, e, e, bad, o, f, m, alert, m, m, b, b, busy)
	}
	if p.play {
		fmt.Fprintf(&js, `const %s=%s("[data-%s]");%s&&%s.addEventListener("click",()=>{%s("videos/"+%s.dataset.%s+"/play",{method:"POST"}).catch(()=>{})});`,
			b, Q, p.playKey, b, b, R, b, jsCamel(p.playKey))
	}
	if p.os {
		fmt.Fprintf(&js, `const %s=navigator.userAgent,%s=/Mac/.test(%s)&&!/iPhone|iPad/.test(%s)?"mac":/Win/.test(%s)?"win":/Linux|X11/.test(%s)&&!/Android/.test(%s)?"linux":"";if(%s){const %s=%s('[data-%s="'+%s+'"]');%s&&(%s.classList.add(%q),%s.parentNode.prepend(%s))}`,
			o, k, o, o, o, o, o, k, c, Q, p.osKey, k, c, c, g.c("on"), c, c)
	}
	if p.latest != "" {
		fmt.Fprintf(&js, `%s(%q).catch(()=>{});`, R, p.latest)
	}
	js.WriteString(`})();`)
	s.js = g.sk.dir + g.pick([]string{"app", "main", "index", "site", "bundle"}) + "." + g.id(8) + ".js"
	s.add(s.js, jsType, []byte(js.String()))
}

// jsCamel data- 属性名在 dataset 里的写法(连字符后面的字母大写)
func jsCamel(k string) string {
	parts := strings.Split(k, "-")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

// subscribeAPI 邮件订阅(博客、软件下载页):邮箱格式不对 422,其余一律「去邮箱确认」
func (s *Site) subscribeAPI(g *gen, path string, form func(msg string, ok bool) []byte) {
	okMsg := g.pick([]string{"Thanks! Check your inbox to confirm your subscription.", "Almost done: we've sent you an email to confirm your subscription.", "Thanks for subscribing. Please confirm from the email we just sent."})
	s.postAPI(path, false, func(in map[string]string) apiReply {
		if !emailRe.MatchString(strings.TrimSpace(in["email"])) {
			return apiReply{code: http.StatusUnprocessableEntity, err: "validation_failed", msg: "Please enter a valid email address.", fields: map[string]string{"email": "Please enter a valid email address."}}
		}
		return apiReply{code: http.StatusOK, ok: true, msg: okMsg}
	}, form)
}

// subscribeBox 邮件订阅的小表单(博客文末、软件下载页)
func subscribeBox(g *gen, s *Site, api, title, note string) string {
	return fmt.Sprintf(`<div class="%s"><b>%s</b><p class="%s">%s</p><form method="post" action="%s" data-%s="%s" novalidate>%s<div class="%s"><input name="email" type="email" autocomplete="email" placeholder="you@example.com" aria-label="Email" required><button type="submit">Subscribe</button></div></form></div>`,
		g.c("subs"), esc(title), g.c("muted"), esc(note), s.apiPre+api, g.sk.formKey, api, emptyAlert(g), g.c("srow"))
}

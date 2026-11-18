package camo

// 伪装站里的图(用户 11-18:视频站等至少要有封面):视频封面、文章题图、头像、软件图标、镜像站的流量图。
// 全是按种子现画的矢量图(SVG),一张几百字节到两三 KB,跟页面一起每周生成一次、放在内存里(开着页面伪装才生成);
// 有图包照片时封面、题图换成照片(media.go),这里画的是没有照片时的兜底。
// 不读磁盘、不连外网、不带脚本和文字。构图(山的走势、楼的高低、星星和窗灯的位置、配色)按种子变,
// 同一张图不会出现在两台服务器上。

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ---- 颜色 ----

type rgb struct{ r, g, b float64 }

func hexRGB(s string) rgb {
	v, _ := strconv.ParseUint(strings.TrimPrefix(s, "#"), 16, 32)
	return rgb{float64(v >> 16 & 0xff), float64(v >> 8 & 0xff), float64(v & 0xff)}
}

func (c rgb) hex() string {
	clamp := func(x float64) int { return int(math.Max(0, math.Min(255, x)) + .5) }
	return fmt.Sprintf("#%02x%02x%02x", clamp(c.r), clamp(c.g), clamp(c.b))
}

// mix a、b 两色按 t 混合(t=0 是 a,t=1 是 b)
func mix(a, b string, t float64) string {
	x, y := hexRGB(a), hexRGB(b)
	return rgb{x.r + (y.r-x.r)*t, x.g + (y.g-x.g)*t, x.b + (y.b-x.b)*t}.hex()
}

// hslHex HSL(0–360、0–100、0–100)转成 #rrggbb
func hslHex(h, s, l float64) string {
	h, s, l = math.Mod(math.Mod(h, 360)+360, 360)/360, s/100, l/100
	q := l * (1 + s)
	if l >= .5 {
		q = l + s - l*s
	}
	p := 2*l - q
	ch := func(t float64) float64 {
		t = math.Mod(t+1, 1)
		switch {
		case t < 1.0/6:
			return p + (q-p)*6*t
		case t < .5:
			return q
		case t < 2.0/3:
			return p + (q-p)*(2.0/3-t)*6
		}
		return p
	}
	return rgb{ch(h+1.0/3) * 255, ch(h) * 255, ch(h-1.0/3) * 255}.hex()
}

// ---- 画布 ----

func ff(x float64) string { return strings.TrimSuffix(strconv.FormatFloat(x, 'f', 1, 64), ".0") }

type svgw struct {
	b    strings.Builder
	g    *gen
	w, h float64
	idp  string // 渐变 id 的前缀:嵌在别的图里画时不和外层撞
	ids  int
}

func newSVG(g *gen, w, h float64) *svgw {
	s := &svgw{g: g, w: w, h: h, idp: "a"}
	fmt.Fprintf(&s.b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %s %s" width="%s" height="%s">`, ff(w), ff(h), ff(w), ff(h))
	return s
}

func (s *svgw) bytes() []byte { return []byte(s.b.String() + "</svg>") }

func (s *svgw) f() float64 { return s.g.r.Float64() }

// lin 线性渐变(x2、y2 是方向:0,1 竖直;1,0 水平),返回 fill 里用的 url(#…)。颜色写成 "#rrggbb@0.4" 带透明度
func (s *svgw) lin(x2, y2 float64, stops ...string) string {
	s.ids++
	id := s.idp + strconv.Itoa(s.ids)
	fmt.Fprintf(&s.b, `<defs><linearGradient id="%s" x2="%s" y2="%s">`, id, ff(x2), ff(y2))
	for i, c := range stops {
		col, alpha, _ := strings.Cut(c, "@")
		fmt.Fprintf(&s.b, `<stop offset="%s" stop-color="%s"`, ff(float64(i)/math.Max(1, float64(len(stops)-1))), col)
		if alpha != "" {
			fmt.Fprintf(&s.b, ` stop-opacity="%s"`, alpha)
		}
		s.b.WriteString("/>")
	}
	s.b.WriteString("</linearGradient></defs>")
	return "url(#" + id + ")"
}

func op(x float64) string { return ` opacity="` + strconv.FormatFloat(x, 'f', 2, 64) + `"` }

func (s *svgw) rect(x, y, w, h float64, fill, more string) {
	fmt.Fprintf(&s.b, `<rect x="%s" y="%s" width="%s" height="%s" fill="%s"%s/>`, ff(x), ff(y), ff(w), ff(h), fill, more)
}

func (s *svgw) circle(cx, cy, r float64, fill, more string) {
	fmt.Fprintf(&s.b, `<circle cx="%s" cy="%s" r="%s" fill="%s"%s/>`, ff(cx), ff(cy), ff(r), fill, more)
}

func (s *svgw) path(d, fill, more string) {
	if d != "" {
		fmt.Fprintf(&s.b, `<path d="%s" fill="%s"%s/>`, d, fill, more)
	}
}

// stroke 只描边的路径
func (s *svgw) stroke(d, col string, width float64, more string) {
	if d != "" {
		fmt.Fprintf(&s.b, `<path d="%s" fill="none" stroke="%s" stroke-width="%s" stroke-linecap="round"%s/>`, d, col, ff(width), more)
	}
}

// nested 在 (x,y,w,h) 这一块里按 vw×vh 的坐标画一幅(超出的部分裁掉):软件截图里的照片、视频画面
func (s *svgw) nested(x, y, w, h, vw, vh float64, draw func(*svgw)) {
	sub := &svgw{g: s.g, w: vw, h: vh, idp: s.idp + strconv.Itoa(s.ids) + "n"}
	s.ids++
	draw(sub)
	fmt.Fprintf(&s.b, `<svg x="%s" y="%s" width="%s" height="%s" viewBox="0 0 %s %s" preserveAspectRatio="xMidYMid slice">%s</svg>`,
		ff(x), ff(y), ff(w), ff(h), ff(vw), ff(vh), sub.b.String())
}

// ---- 场景的零件 ----

// palette 一天里不同时候的天色:天顶、地平线、太阳、远山、近处
type palette struct{ top, bot, sun, far, near string }

var palettes = map[string]palette{
	"dawn":  {"#e9a48f", "#fde1c3", "#fff2d8", "#c58f97", "#55405c"},
	"day":   {"#5c9ed6", "#d3e9f7", "#fffbe8", "#86a9c2", "#2c5a4d"},
	"dusk":  {"#2f2756", "#ef8d5b", "#ffd391", "#76507a", "#271d3a"},
	"night": {"#0a1430", "#26406f", "#f1f4ff", "#283c66", "#0c1325"},
	"rain":  {"#71839a", "#b9c4d0", "#e8edf2", "#7d8a99", "#3b4552"},
}

func (s *svgw) sky(p palette) { s.rect(0, 0, s.w, s.h, s.lin(0, 1, p.top, p.bot), "") }

func (s *svgw) sun(cx, cy, r float64, col string) {
	s.circle(cx, cy, r*2.7, col, op(.12))
	s.circle(cx, cy, r*1.65, col, op(.22))
	s.circle(cx, cy, r, col, "")
}

func (s *svgw) stars(n int, maxY float64) {
	var a, b strings.Builder
	for i := 0; i < n; i++ {
		x, y, r := s.w*s.f(), maxY*s.f(), .6+s.f()*.8
		d := &a
		if i%3 == 0 {
			d = &b
		}
		fmt.Fprintf(d, "M%s %sh%sv%sh-%sz", ff(x), ff(y), ff(r), ff(r), ff(r))
	}
	s.path(a.String(), "#ffffff", op(.85))
	s.path(b.String(), "#ffffff", op(.45))
}

type pt struct{ x, y float64 }

// ridge 低多边形的山脊:一高一低往右走,y0 是平均高度,amp 是起伏;返回山脊的点(画雪顶、倒影用)
func (s *svgw) ridge(y0, amp, step float64, fill, more string) []pt {
	pts := []pt{{0, y0 + amp*(s.f()-.5)}}
	x, up := 0.0, s.f() < .5
	for x < s.w {
		x = math.Min(s.w, x+step*(.55+s.f()*.9))
		y := y0 + amp*.45*s.f()
		if up {
			y = y0 - amp*(.35+.65*s.f())
		}
		up = !up
		pts = append(pts, pt{x, y})
	}
	s.path(polyD(pts, s.h), fill, more)
	return pts
}

// polyD 从左下角起,沿 pts 走到右边再回到底边的闭合路径
func polyD(pts []pt, bottom float64) string {
	var d strings.Builder
	fmt.Fprintf(&d, "M%s %s", ff(pts[0].x), ff(bottom))
	for _, p := range pts {
		fmt.Fprintf(&d, "L%s %s", ff(p.x), ff(p.y))
	}
	fmt.Fprintf(&d, "L%s %sZ", ff(pts[len(pts)-1].x), ff(bottom))
	return d.String()
}

// hills 平缓起伏(沙丘、丘陵、雪原)
func (s *svgw) hills(y0, amp, step float64, fill, more string) {
	var d strings.Builder
	fmt.Fprintf(&d, "M0 %sL0 %sQ%s %s %s %s", ff(s.h), ff(y0+amp*(s.f()-.5)), ff(step*.5), ff(y0-amp*s.f()), ff(step), ff(y0+amp*(s.f()-.5)))
	for x := step * 2; x < s.w+step; x += step {
		fmt.Fprintf(&d, "T%s %s", ff(x), ff(y0+amp*(s.f()-.5)))
	}
	fmt.Fprintf(&d, "L%s %sZ", ff(s.w+step), ff(s.h))
	s.path(d.String(), fill, more)
}

// pines 一排松树(三角),连同树下的地面一直铺到底
func (s *svgw) pines(base, hgt float64, n int, col string) {
	var d strings.Builder
	for i := 0; i < n; i++ {
		x := s.w * (float64(i) + s.f()) / float64(n)
		th := hgt * (.6 + .6*s.f())
		y := base + hgt*.2*s.f()
		fmt.Fprintf(&d, "M%s %sL%s %sL%s %sZ", ff(x), ff(y-th), ff(x-th*.21), ff(y), ff(x+th*.21), ff(y))
	}
	fmt.Fprintf(&d, "M0 %sH%sV%sH0Z", ff(base+hgt*.12), ff(s.w), ff(s.h))
	s.path(d.String(), col, "")
}

// skyline 一排楼,lit 时窗户亮着
func (s *svgw) skyline(base, maxH float64, col string, lit bool) {
	var d, win strings.Builder
	for x := -4.0; x < s.w; {
		bw, bh := s.w*(.045+.06*s.f()), maxH*(.35+.65*s.f())
		top := base - bh
		fmt.Fprintf(&d, "M%s %sH%sV%sH%sZ", ff(x), ff(s.h), ff(x+bw), ff(top), ff(x))
		if s.f() < .22 {
			fmt.Fprintf(&d, "M%s %sh1.4v-%sh-1.4z", ff(x+bw*.5), ff(top), ff(s.h*.05))
		}
		if lit {
			for k, n := 0, 3+s.g.r.IntN(7); k < n; k++ {
				fmt.Fprintf(&win, "M%s %sh2.2v3h-2.2z", ff(x+2+(bw-5)*s.f()), ff(top+4+(bh-8)*s.f()))
			}
		}
		x += bw + .6
	}
	s.path(d.String(), col, "")
	s.path(win.String(), "#ffd47a", op(.9))
}

// water 从地平线 hz 铺到底的水面,带太阳的倒影和几道波光
func (s *svgw) water(hz float64, p palette, sunX float64) {
	s.rect(0, hz, s.w, s.h-hz, s.lin(0, 1, mix(p.bot, p.far, .45), mix(p.near, "#000000", .25)), "")
	if sunX >= 0 {
		for i := 0; i < 7; i++ {
			w := s.w * (.15 - .017*float64(i))
			s.rect(sunX-w/2, hz+3+float64(i)*(s.h-hz)*.13, w, 1.5, p.sun, op(.55-.06*float64(i)))
		}
	}
	var d strings.Builder
	for i := 0; i < 22; i++ {
		fmt.Fprintf(&d, "M%s %sh%s", ff(s.w*s.f()), ff(hz+4+(s.h-hz-6)*s.f()), ff(5+11*s.f()))
	}
	s.stroke(d.String(), mix(p.bot, "#ffffff", .3), .9, op(.35))
}

// ---- 场景 ----

// cover 画一张 w×h 的封面 / 题图。kind 是场景(海边、城市、山……),可以写成 "sea:dusk" 带上天色;tod 不空时用它
func cover(g *gen, kind, tod string, w, h float64) []byte {
	s := newSVG(g, w, h)
	if k, t, ok := strings.Cut(kind, ":"); ok {
		kind = k
		if tod == "" {
			tod = t
		}
	}
	if kind == "" {
		kind, tod = g.pick([]string{"mountain", "sea", "forest", "lake"}), g.pick([]string{"dawn", "day", "dusk"})
	}
	s.scene(kind, tod)
	return s.bytes()
}

// trafficChart 镜像首页「最近 30 天流量」的面积图(不带字);返回图和峰值、平均值(TB / 天)
func trafficChart(g *gen, w, h float64) ([]byte, float64, float64) {
	s := newSVG(g, w, h)
	acc := g.accent()
	n, base := 30, 1.2+1.6*g.r.Float64()
	vals := make([]float64, n)
	peak, sum := 0.0, 0.0
	for i := range vals {
		wk := 1.0
		if i%7 == 5 || i%7 == 6 { // 周末多一点
			wk = 1.18
		}
		v := base * wk * (0.82 + 0.36*g.r.Float64()) * (0.9 + 0.2*float64(i)/float64(n))
		vals[i] = v
		sum += v
		if v > peak {
			peak = v
		}
	}
	top := peak * 1.15
	x := func(i int) float64 { return 4 + (w-8)*float64(i)/float64(n-1) }
	y := func(v float64) float64 { return h - 6 - (h-16)*v/top }
	for k := 1; k <= 3; k++ { // 三条淡淡的横线
		s.rect(0, h-6-(h-16)*float64(k)/4, w, 1, "#e9ecf1", "")
	}
	var line strings.Builder
	for i, v := range vals {
		cmd := "L"
		if i == 0 {
			cmd = "M"
		}
		fmt.Fprintf(&line, "%s%s %s", cmd, ff(x(i)), ff(y(v)))
	}
	area := fmt.Sprintf("%sL%s %sL%s %sZ", line.String(), ff(x(n-1)), ff(h-6), ff(x(0)), ff(h-6))
	s.path(area, s.lin(0, 1, acc+"@0.28", acc+"@0.02"), "")
	s.stroke(line.String(), acc, 2, ` stroke-linejoin="round"`)
	s.circle(x(n-1), y(vals[n-1]), 3.5, acc, "")
	return s.bytes(), peak, sum / float64(n)
}

func (s *svgw) scene(kind, tod string) {
	p, ok := palettes[tod]
	if !ok {
		p = palettes["day"]
	}
	W, H, f := s.w, s.h, s.f
	switch kind {
	case "mountain":
		s.sky(p)
		s.sun(W*(.2+.6*f()), H*(.2+.16*f()), H*.07, p.sun)
		s.ridge(H*.5, H*.17, W*.11, mix(p.far, p.bot, .4), "")
		s.ridge(H*.63, H*.15, W*.09, p.far, "")
		s.ridge(H*.77, H*.12, W*.08, mix(p.far, p.near, .6), "")
		s.pines(H*.88, H*.1, 16, p.near)
	case "sea", "harbor":
		hz := H * (.56 + .06*f())
		s.sky(p)
		sx := W * (.25 + .5*f())
		s.sun(sx, hz-H*(.07+.12*f()), H*.075, p.sun)
		s.hills(hz+1, H*.025, W*.2, mix(p.far, p.bot, .3), "")
		s.water(hz, p, sx)
		if kind == "sea" { // 一边是伸进海里的岬角
			if f() < .5 {
				s.path(fmt.Sprintf("M0 %sL0 %sL%s %sL%s %sZ", ff(H), ff(hz-H*.2), ff(W*.12), ff(hz-H*.12), ff(W*.32), ff(H)), p.near, "")
			} else {
				s.path(fmt.Sprintf("M%s %sL%s %sL%s %sL%s %sZ", ff(W), ff(H), ff(W), ff(hz-H*.18), ff(W*.86), ff(hz-H*.1), ff(W*.66), ff(H)), p.near, "")
			}
			break
		}
		dark := mix(p.near, "#000000", .3)
		var boats, masts strings.Builder
		for i, n := 0, 2+s.g.r.IntN(2); i < n; i++ {
			x, y, bw := W*(.12+.7*f()), hz+H*(.05+.12*f()), W*(.07+.05*f())
			fmt.Fprintf(&boats, "M%s %sh%sl-%s %sh-%sz", ff(x), ff(y), ff(bw), ff(bw*.15), ff(bw*.18), ff(bw*.7))
			fmt.Fprintf(&masts, "M%s %sv-%s", ff(x+bw*.45), ff(y), ff(bw*.9))
		}
		s.path(boats.String(), dark, "")
		s.stroke(masts.String(), dark, 1.2, "")
		var posts strings.Builder
		for i := 0; i < 9; i++ { // 前景的栈桥桩
			fmt.Fprintf(&posts, "M%s %sh3v%sh-3z", ff(W*.55+float64(i)*W*.05), ff(H*.82-float64(i)*1.5), ff(H*.2))
		}
		s.path(posts.String(), dark, "")
		s.rect(W*.52, H*.8, W*.48, 3, dark, "")
	case "city":
		s.sky(p)
		if tod == "night" {
			s.stars(40, H*.5)
			s.circle(W*(.62+.25*f()), H*.2, H*.06, "#f3f0e2", "")
		} else {
			s.sun(W*(.3+.4*f()), H*.58, H*.1, p.sun)
		}
		s.skyline(H*.66, H*.26, mix(p.far, p.bot, .25), false)
		s.skyline(H*.8, H*.36, p.near, tod == "night" || f() < .5)
	case "town", "market":
		s.sky(p)
		base := H * .78
		var win strings.Builder
		facades := []string{"#d9b48f", "#c98d6f", "#e3d3b1", "#9fb0a8", "#b9a3c7", "#d7a5a0"}
		if kind == "town" {
			facades = []string{"#8f9aa6", "#a3927f", "#7e8d8b", "#998aa0", "#a7a39a"}
		}
		for x := -3.0; x < W; {
			hw, hh := W*(.08+.05*f()), H*(.22+.18*f())
			top := base - hh
			s.path(fmt.Sprintf("M%s %sH%sV%sH%sZ", ff(x), ff(H), ff(x+hw), ff(top), ff(x)), facades[s.g.r.IntN(len(facades))], "")
			s.path(fmt.Sprintf("M%s %sL%s %sL%s %sZ", ff(x-1.5), ff(top+.5), ff(x+hw/2), ff(top-hw*.42), ff(x+hw+1.5), ff(top+.5)), mix(p.near, "#7a3b2e", .5), "")
			for wy := top + 6; wy < base-10; wy += 11 {
				fmt.Fprintf(&win, "M%s %sh3.4v5h-3.4zM%s %sh3.4v5h-3.4z", ff(x+hw*.22), ff(wy), ff(x+hw*.62), ff(wy))
			}
			x += hw + 1
		}
		s.path(win.String(), mix(p.near, "#000000", .2), op(.55))
		s.rect(0, base, W, H-base, mix(p.near, "#2b2b2b", .4), "")
		if kind == "market" { // 前面一排条纹篷子的摊位
			cols := []string{"#d9534f", "#2f8f83", "#e0a43a", "#3f6fb5"}
			for i, x := 0, W*.03; x < W; i++ {
				sw := W * (.17 + .05*f())
				c := cols[(i+s.g.r.IntN(2))%len(cols)]
				s.rect(x, H*.73, sw, H*.2, "#7b5b43", "")
				s.rect(x-2, H*.62, sw+4, H*.1, c, "")
				var st strings.Builder
				for k := 0.0; k < sw; k += 9 {
					fmt.Fprintf(&st, "M%s %sh4.5v%sh-4.5z", ff(x+k), ff(H*.62), ff(H*.1))
				}
				s.path(st.String(), "#ffffff", op(.7))
				var fruit strings.Builder
				for k := 0; k < 7; k++ {
					fmt.Fprintf(&fruit, "M%s %sa2.6 2.6 0 1 0 .1 0z", ff(x+5+(sw-10)*f()), ff(H*.73-2))
				}
				s.path(fruit.String(), g2(s, []string{"#e2574c", "#f0b23e", "#7cb342", "#f08a3c"}), "")
				x += sw + W*.04
			}
		} else { // 下雨
			var d strings.Builder
			for i := 0; i < 70; i++ {
				fmt.Fprintf(&d, "M%s %sl-3 10", ff(W*1.05*f()), ff(H*f()))
			}
			s.stroke(d.String(), "#e6edf5", .8, op(.45))
		}
	case "train":
		s.sky(p)
		if tod == "night" {
			s.stars(42, H*.55)
			s.circle(W*(.15+.2*f()), H*.18, H*.055, "#f3f0e2", "")
		} else {
			s.sun(W*(.6+.25*f()), H*.25, H*.07, p.sun)
		}
		s.hills(H*.6, H*.1, W*.25, mix(p.far, p.bot, .3), "")
		s.hills(H*.7, H*.06, W*.3, p.far, "")
		s.rect(0, H*.8, W, H*.2, p.near, "")
		if tod == "night" { // 一列亮着窗的夜车
			body := mix(p.near, "#000000", .15)
			var win strings.Builder
			x0 := W * (.05 + .1*f())
			for i := 0; i < 5; i++ {
				x := x0 + float64(i)*W*.17
				s.rect(x, H*.68, W*.16, H*.11, body, ` rx="3"`)
				for k := 0; k < 5; k++ {
					fmt.Fprintf(&win, "M%s %sh%sv%sh-%sz", ff(x+4+float64(k)*W*.03), ff(H*.7), ff(W*.02), ff(H*.035), ff(W*.02))
				}
			}
			s.path(win.String(), "#ffd47a", op(.95))
			s.stroke(fmt.Sprintf("M0 %sH%s", ff(H*.8), ff(W)), mix(p.far, "#ffffff", .2), 1.4, "")
		} else { // 伸向远处的旧铁轨
			vx, vy := W*.5, H*.6
			s.path(fmt.Sprintf("M%s %sL%s %sL%s %sL%s %sZ", ff(W*.3), ff(H), ff(vx-2), ff(vy), ff(vx+2), ff(vy), ff(W*.7), ff(H)), mix(p.near, "#8a7a62", .35), "")
			var sl strings.Builder
			for t := .05; t < 1; t += t*.35 + .03 {
				y := vy + (H-vy)*t
				fmt.Fprintf(&sl, "M%s %sH%s", ff(vx-(W*.24)*t), ff(y), ff(vx+(W*.24)*t))
			}
			s.stroke(sl.String(), "#5a4636", 1.6, "")
			s.stroke(fmt.Sprintf("M%s %sL%s %sM%s %sL%s %s", ff(W*.36), ff(H), ff(vx-1), ff(vy), ff(W*.64), ff(H), ff(vx+1), ff(vy)), "#c9c3b8", 1.5, "")
		}
	case "forest":
		s.sky(p)
		s.sun(W*(.3+.4*f()), H*.3, H*.08, p.sun)
		s.pines(H*.5, H*.18, 30, mix(p.far, p.bot, .45))
		s.rect(0, H*.42, W, H*.2, s.lin(0, 1, "#ffffff@0", "#ffffff@0.45", "#ffffff@0"), "")
		s.pines(H*.66, H*.24, 22, mix(p.far, p.near, .35))
		s.pines(H*.88, H*.36, 13, mix(p.near, "#000000", .2))
	case "desert":
		s.sky(p)
		s.sun(W*(.25+.5*f()), H*(.25+.15*f()), H*.08, p.sun)
		sand := "#e2b07a"
		if tod == "dusk" {
			sand = "#c9875e"
		}
		s.hills(H*.6, H*.05, W*.33, mix(sand, p.bot, .35), "")
		s.hills(H*.7, H*.07, W*.28, sand, "")
		s.hills(H*.85, H*.06, W*.4, mix(sand, "#7a4a2a", .3), "")
		vx := W * (.45 + .1*f())
		s.path(fmt.Sprintf("M%s %sL%s %sL%s %sL%s %sZ", ff(W*.28), ff(H), ff(vx-1.5), ff(H*.6), ff(vx+1.5), ff(H*.6), ff(W*.72), ff(H)), "#3d3a38", "")
		var dash strings.Builder
		for t := .06; t < 1; t += t*.5 + .04 {
			y := H*.6 + H*.4*t
			x := vx + (W*.5-vx)*t
			fmt.Fprintf(&dash, "M%s %sl%s %s", ff(x), ff(y), ff((W*.5-vx)*t*.15), ff(H*.4*t*.15))
		}
		s.stroke(dash.String(), "#f2d27a", 1.6, "")
	case "lake":
		s.sky(p)
		pk := s.ridge(H*.42, H*.18, W*.1, "#9fb3c7", "")
		s.snowCaps(pk)
		s.ridge(H*.56, H*.1, W*.13, "#7b93a6", "")
		lk := H * .66
		s.hills(lk-H*.02, H*.015, W*.22, "#eef3f7", "") // 湖岸的雪
		s.rect(0, lk, W, H-lk, s.lin(0, 1, "#a9c8dc", "#5f86a3"), "")
		mirror := make([]pt, len(pk)) // 远山在湖里的倒影(压扁一点、淡一点)
		for i, q := range pk {
			mirror[i] = pt{q.x, lk + (lk-q.y)*.8}
		}
		s.path(polyD(mirror, lk), "#c3d3e0", op(.35))
		var d strings.Builder
		for i := 0; i < 12; i++ {
			fmt.Fprintf(&d, "M%s %sh%s", ff(W*f()), ff(lk+8+(H-lk-10)*f()), ff(8+14*f()))
		}
		s.stroke(d.String(), "#ffffff", .9, op(.45))
	case "island":
		hz := H * .56
		s.sky(p)
		sx := W * (.6 + .3*f())
		s.sun(sx, H*.2, H*.065, p.sun)
		s.water(hz, palette{p.top, "#bfe3ee", p.sun, "#4aa3b8", "#1f6f86"}, -1)
		for i, n := 0, 2+s.g.r.IntN(2); i < n; i++ {
			x, iw, ih := W*(.08+.6*f()), W*(.16+.14*f()), H*(.08+.08*f())
			y := hz + H*(.02+.06*float64(i))
			s.path(fmt.Sprintf("M%s %sQ%s %s %s %sZ", ff(x-4), ff(y+2), ff(x+iw/2), ff(y-ih*.6), ff(x+iw+4), ff(y+2)), "#ead7a4", "")
			s.path(fmt.Sprintf("M%s %sQ%s %s %s %sZ", ff(x), ff(y), ff(x+iw/2), ff(y-ih*2), ff(x+iw), ff(y)), mix("#3f8f4f", "#1e5d3a", f()), "")
			if i == 0 {
				tx, ty := x+iw*.62, y-ih*.55
				s.stroke(fmt.Sprintf("M%s %sq3 -9 1 -18", ff(tx), ff(ty)), "#6b4a2f", 2, "")
				s.stroke(fmt.Sprintf("M%s %sq-7 -1 -11 5M%s %sq7 -2 11 4M%s %sq-4 -6 -10 -6M%s %sq5 -6 10 -5", ff(tx+1), ff(ty-18), ff(tx+1), ff(ty-18), ff(tx+1), ff(ty-18), ff(tx+1), ff(ty-18)), "#2f7a3f", 2.2, "")
			}
		}
	case "clouds":
		s.sky(p)
		s.sun(W*(.2+.6*f()), H*.42, H*.07, p.sun)
		pk := s.ridge(H*.5, H*.2, W*.12, mix(p.far, p.near, .5), "")
		s.snowCaps(pk)
		var d strings.Builder
		for i := 0; i < 26; i++ {
			cx, cy, rx := W*1.1*f()-W*.05, H*(.6+.12*f()), W*(.07+.08*f())
			fmt.Fprintf(&d, "M%s %sa%s %s 0 1 0 %s 0a%s %s 0 1 0 -%s 0z", ff(cx-rx), ff(cy), ff(rx), ff(rx*.36), ff(rx*2), ff(rx), ff(rx*.36), ff(rx*2))
		}
		s.path(d.String(), "#fff6ef", op(.92))
		s.rect(0, H*.7, W, H*.3, "#fbefe7", "")
	case "studio":
		s.rect(0, 0, W, H, s.lin(0, 1, "#22242e", "#0d0e13"), "")
		hue := float64(s.g.hue)
		for i := 0; i < 9; i++ {
			s.circle(W*f(), H*.55*f(), H*(.04+.06*f()), hslHex(hue+float64(i)*35, 70, 60), op(.16))
		}
		beam := s.lin(0, 1, "#ffffff@0.30", "#ffffff@0")
		for i, n := 0, 2+s.g.r.IntN(2); i < n; i++ {
			x := W * (.18 + .64*float64(i)/math.Max(1, float64(n-1)))
			s.path(fmt.Sprintf("M%s -2L%s -2L%s %sL%s %sZ", ff(x-4), ff(x+4), ff(x+W*.16), ff(H*.86), ff(x-W*.16), ff(H*.86)), beam, "")
		}
		s.path(fmt.Sprintf("M%s %sa%s %s 0 1 0 %s 0a%s %s 0 1 0 -%s 0z", ff(W*.15), ff(H*.87), ff(W*.35), ff(H*.05), ff(W*.7), ff(W*.35), ff(H*.05), ff(W*.7)), "#000000", op(.35))
		col := "#4a4e5c"
		if f() < .5 { // 麦克风架
			mx := W * (.4 + .2*f())
			s.stroke(fmt.Sprintf("M%s %sV%sl%s -%s", ff(mx), ff(H*.86), ff(H*.5), ff(W*.06), ff(H*.08)), col, 2.4, "")
			s.rect(mx+W*.055, H*.33, W*.03, H*.1, "#8a8f9e", ` rx="4"`)
			s.stroke(fmt.Sprintf("M%s %sl-%s %sM%s %sl%s %s", ff(mx), ff(H*.86), ff(W*.05), ff(H*.02), ff(mx), ff(H*.86), ff(W*.05), ff(H*.02)), col, 2, "")
		} else { // 三脚架上的相机
			cx := W * (.42 + .16*f())
			s.stroke(fmt.Sprintf("M%s %sL%s %sM%s %sL%s %sM%s %sV%s", ff(cx), ff(H*.52), ff(cx-W*.07), ff(H*.86), ff(cx), ff(H*.52), ff(cx+W*.07), ff(H*.86), ff(cx), ff(H*.52), ff(H*.86)), col, 2, "")
			s.rect(cx-W*.06, H*.38, W*.12, H*.14, "#2d303a", ` rx="3"`)
			s.circle(cx+W*.035, H*.45, H*.045, "#5b6170", "")
			s.circle(cx+W*.035, H*.45, H*.022, "#9aa3b5", "")
		}
	case "rack":
		s.rect(0, 0, W, H, s.lin(1, 1, "#1d2733", "#0e141b"), "")
		x0, rw := W*.3, W*.4
		s.rect(x0-6, H*.08, rw+12, H*.88, "#2b3542", ` rx="4"`)
		y := H * .12
		var leds, vents strings.Builder
		for y < H*.88 {
			uh := H * (.07 + .05*float64(s.g.r.IntN(2)))
			s.rect(x0, y, rw, uh-2, mix("#3a4656", "#2c3644", f()), ` rx="2"`)
			for k := 0; k < 3; k++ {
				fmt.Fprintf(&leds, "M%s %sh2.4v2.4h-2.4z", ff(x0+6+float64(k)*6), ff(y+uh/2-2))
			}
			for k := 0; k < 6; k++ {
				fmt.Fprintf(&vents, "M%s %sv%s", ff(x0+rw*.55+float64(k)*5), ff(y+3), ff(uh-8))
			}
			y += uh
		}
		s.path(leds.String(), "#5ee08a", "")
		s.stroke(vents.String(), "#1b222c", 1.2, "")
		s.circle(W*.82, H*.25, H*.12, "#5ee08a", op(.08))
	case "film":
		s.rect(0, 0, W, H, s.lin(1, 1, hslHex(float64(s.g.hue), 55, 72), hslHex(float64(s.g.hue+50), 60, 55)), "")
		fmt.Fprintf(&s.b, `<g transform="rotate(-8 %s %s)">`, ff(W/2), ff(H/2))
		s.rect(-20, H*.3, W+40, H*.4, "#191919", "")
		var holes strings.Builder
		for x := -16.0; x < W+20; x += 11 {
			fmt.Fprintf(&holes, "M%s %sh5v4h-5zM%s %sh5v4h-5z", ff(x), ff(H*.315), ff(x), ff(H*.645))
		}
		s.path(holes.String(), "#efe6d6", op(.9))
		for i := 0; i < 4; i++ {
			fx := W*.04 + float64(i)*W*.25
			s.nested(fx, H*.37, W*.22, H*.26, 160, 110, func(n *svgw) {
				n.scene(n.g.pick([]string{"sea", "mountain", "forest"}), n.g.pick([]string{"dawn", "day", "dusk"}))
			})
		}
		s.b.WriteString("</g>")
	case "prints":
		s.rect(0, 0, W, H, "#e8e1d4", "")
		for i := 0; i < 3; i++ {
			cx, cy := W*(.25+.25*float64(i)), H*(.48+.08*(f()-.5))
			pw, ph := W*.3, H*.42
			fmt.Fprintf(&s.b, `<g transform="rotate(%s %s %s)">`, ff((f()-.5)*18), ff(cx), ff(cy))
			s.rect(cx-pw/2+3, cy-ph/2+4, pw, ph, "#000000", op(.12))
			s.rect(cx-pw/2, cy-ph/2, pw, ph, "#fdfcf9", "")
			s.nested(cx-pw/2+5, cy-ph/2+5, pw-10, ph-17, 160, 110, func(n *svgw) {
				n.scene(n.g.pick([]string{"sea", "mountain", "lake", "desert"}), n.g.pick([]string{"dawn", "day", "dusk"}))
			})
			s.b.WriteString("</g>")
		}
	case "notebook":
		s.rect(0, 0, W, H, s.lin(1, 1, "#c79f79", "#9d734e"), "")
		var grain strings.Builder
		for i := 0; i < 14; i++ {
			fmt.Fprintf(&grain, "M0 %sQ%s %s %s %s", ff(H*f()), ff(W*.5), ff(H*f()), ff(W), ff(H*f()))
		}
		s.stroke(grain.String(), "#7d5638", .8, op(.25))
		fmt.Fprintf(&s.b, `<g transform="rotate(%s %s %s)">`, ff(-4+8*f()), ff(W/2), ff(H/2))
		s.rect(W*.2+3, H*.12+4, W*.5, H*.8, "#000000", op(.18))
		s.rect(W*.2, H*.12, W*.5, H*.8, "#fbf6ea", ` rx="3"`)
		var lines strings.Builder
		for y := H * .22; y < H*.88; y += H * .065 {
			fmt.Fprintf(&lines, "M%s %sH%s", ff(W*.23), ff(y), ff(W*.67))
		}
		s.stroke(lines.String(), "#b8c8dc", .8, "")
		s.stroke(fmt.Sprintf("M%s %sV%s", ff(W*.27), ff(H*.14), ff(H*.9)), "#e0a3a3", .9, "")
		var ink strings.Builder
		for y := H * .22; y < H*.62; y += H * .065 {
			fmt.Fprintf(&ink, "M%s %sh%s", ff(W*.29), ff(y-2), ff(W*(.1+.25*f())))
		}
		s.stroke(ink.String(), "#3d4a66", 1.1, op(.6))
		s.b.WriteString("</g>")
		fmt.Fprintf(&s.b, `<g transform="rotate(32 %s %s)">`, ff(W*.82), ff(H*.5))
		s.rect(W*.8, H*.12, W*.025, H*.62, "#24324a", ` rx="2"`)
		s.b.WriteString("</g>")
		s.circle(W*.86, H*.82, H*.11, "none", ` stroke="#6b4a2f" stroke-width="2.5"`+op(.35))
	case "bike":
		s.rect(0, 0, W, H, s.lin(0, 1, hslHex(float64(s.g.hue), 45, 90), hslHex(float64(s.g.hue+20), 40, 80)), "")
		gy := H * .8
		s.rect(0, gy, W, H-gy, hslHex(float64(s.g.hue+20), 30, 70), "")
		r := H * .19
		rx, fx, by := W*.31, W*.69, gy-r
		ink := "#2a2f3a"
		s.circle(rx, by, r, "none", ` stroke="`+ink+`" stroke-width="3.2"`)
		s.circle(fx, by, r, "none", ` stroke="`+ink+`" stroke-width="3.2"`)
		bb := pt{W * .47, by}
		seat, head := pt{W * .43, by - r*1.15}, pt{W * .62, by - r*1.05}
		frame := fmt.Sprintf("M%s %sL%s %sL%s %sL%s %sL%s %sM%s %sL%s %sM%s %sl-%s 0M%s %sl%s -%s",
			ff(rx), ff(by), ff(bb.x), ff(bb.y), ff(head.x), ff(head.y), ff(seat.x), ff(seat.y), ff(rx), ff(by),
			ff(seat.x), ff(seat.y), ff(bb.x), ff(bb.y), ff(seat.x), ff(seat.y-6), ff(W*.03), ff(head.x), ff(head.y), ff(W*.02), ff(H*.05))
		s.stroke(frame, hslHex(float64(s.g.hue+180), 60, 45), 3, ` stroke-linejoin="round"`)
		s.stroke(fmt.Sprintf("M%s %sL%s %s", ff(head.x), ff(head.y), ff(fx), ff(by)), hslHex(float64(s.g.hue+180), 60, 45), 3, "")
		s.circle(bb.x, bb.y, r*.22, "none", ` stroke="`+ink+`" stroke-width="2"`)
	case "disc":
		s.rect(0, 0, W, H, s.lin(1, 1, "#1b1d26", "#2c2f3d"), "")
		cx, cy, r := W*.5, H*.5, H*.4
		s.circle(cx+4, cy+6, r, "#000000", op(.3))
		s.circle(cx, cy, r, s.lin(1, 1, "#dfe7f3", "#f1d6ea", "#d4f0e3", "#f5ecd0", "#d9e3f5"), "")
		s.circle(cx, cy, r*.34, "#c9d0db", "")
		s.circle(cx, cy, r*.13, "#232633", "")
		s.stroke(fmt.Sprintf("M%s %sA%s %s 0 0 1 %s %s", ff(cx-r*.75), ff(cy-r*.45), ff(r*.88), ff(r*.88), ff(cx+r*.2), ff(cy-r*.86)), "#ffffff", 3, op(.5))
	default: // flatlay:桌上摊开的几样小东西
		s.rect(0, 0, W, H, hslHex(float64(s.g.hue), 30, 88), "")
		items := 0
		for items < 6 {
			x, y := W*(.08+.7*f()), H*(.1+.62*f())
			w, h := W*(.1+.14*f()), H*(.12+.22*f())
			c := hslHex(float64(s.g.hue+60*items), 35, 40+30*f())
			s.rect(x+3, y+4, w, h, "#000000", ` rx="5"`+op(.13))
			if items%3 == 2 {
				s.circle(x+w/2, y+h/2, math.Min(w, h)/2, c, "")
			} else {
				s.rect(x, y, w, h, c, ` rx="5"`)
			}
			items++
		}
	}
}

// snowCaps 在山脊的每个山尖上加一块雪顶(沿两侧山坡往下三成)
func (s *svgw) snowCaps(pk []pt) {
	var d strings.Builder
	for i := 1; i+1 < len(pk); i++ {
		c, l, r := pk[i], pk[i-1], pk[i+1]
		if c.y >= l.y || c.y >= r.y {
			continue
		}
		at := func(o pt, t float64) pt { return pt{c.x + (o.x-c.x)*t, c.y + (o.y-c.y)*t} }
		a, b, m1, m2 := at(l, .32), at(r, .3), at(l, .18), at(r, .16)
		fmt.Fprintf(&d, "M%s %sL%s %sL%s %sL%s %sL%s %sL%s %sZ", ff(c.x), ff(c.y), ff(a.x), ff(a.y), ff(m1.x+2), ff(m1.y+3), ff(c.x), ff(c.y+(a.y-c.y)*.55), ff(m2.x-2), ff(m2.y+3), ff(b.x), ff(b.y))
	}
	s.path(d.String(), "#f7fafc", "")
}

func g2(s *svgw, list []string) string { return list[s.g.r.IntN(len(list))] }

// ---- 头像、软件图标、软件截图 ----

// avatar 个人站的头像:渐变底上一个人的剪影(不写字)
func avatar(g *gen) []byte {
	s := newSVG(g, 96, 96)
	s.rect(0, 0, 96, 96, s.lin(1, 1, hslHex(float64(g.hue), 55, 62), hslHex(float64(g.hue+40), 60, 42)), "")
	s.circle(48, 38, 17, "#ffffff", op(.92))
	s.path("M14 96C16 70 31 60 48 60S80 70 82 96Z", "#ffffff", op(.92))
	return s.bytes()
}

// appIcon 软件的图标:圆角方块渐变底 + 按产品类别的白色图形
func appIcon(g *gen, kind string) []byte {
	s := newSVG(g, 64, 64)
	acc := hslHex(float64(g.hue), 62, 48)
	s.rect(0, 0, 64, 64, s.lin(1, 1, mix(acc, "#ffffff", .15), mix(acc, "#000000", .25)), ` rx="15"`)
	w := "#ffffff"
	switch kind {
	case "Studio":
		s.circle(32, 33, 16, "none", ` stroke="`+w+`" stroke-width="4"`)
		s.circle(26, 28, 3.2, w, "")
		s.circle(36, 26, 3.2, w, "")
		s.circle(40, 35, 3.2, w, "")
	case "Sync":
		s.stroke("M20 30a12 12 0 0 1 21-7M44 34a12 12 0 0 1-21 7", w, 4, "")
		s.path("M43 15v10h-10zM21 49V39h10z", w, "")
	case "Desk":
		s.rect(15, 17, 34, 23, "none", ` stroke="`+w+`" stroke-width="4" rx="3"`)
		s.stroke("M32 41v7M24 49h16", w, 4, "")
	case "Capture":
		s.rect(13, 21, 38, 26, "none", ` stroke="`+w+`" stroke-width="4" rx="5"`)
		s.circle(32, 34, 7, "none", ` stroke="`+w+`" stroke-width="4"`)
		s.path("M24 21l3-5h10l3 5z", w, "")
	case "Notes":
		s.rect(18, 13, 28, 38, "none", ` stroke="`+w+`" stroke-width="4" rx="3"`)
		s.stroke("M25 24h14M25 32h14M25 40h9", w, 3.5, "")
	case "Player":
		s.path("M25 18v28l22-14z", w, "")
	default: // Draw
		s.stroke("M19 45L41 23l5 5-22 22H19z", w, 4, ` stroke-linejoin="round"`)
		s.stroke("M37 27l5 5", w, 4, "")
	}
	return s.bytes()
}

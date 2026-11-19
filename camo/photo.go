package camo

// 图包照片的处理(见 media.go):每台服务器按自己的种子把原图处理成伪装站用的尺寸。
// 取景(放大 0–18% 后在画面里挪)、左右翻转、亮度 / 对比度 / 饱和度 / 色温、轻微暗角都按种子定,
// 再按 74–86 之间的质量重新压缩,不带任何元数据。同一种子、同一张图的各个尺寸取景和色调一致
// (列表里的小图和详情页的大图是同一个画面),结果每次一样(重建站点时字节不变)。
// 逐行缩放:原图解码后直接按需取像素,不另开整张的中间图,一张 1280×720 处理时只多占几 MB。

import (
	"bytes"
	"errors"
	"hash/fnv"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"math/rand/v2"
)

var (
	errPhotoSize  = errors.New("photo: output size out of range")
	errPhotoImage = errors.New("photo: source image size out of range")
)

// photoLook 一张照片在这台服务器上的样子(只看种子和文件名)
type photoLook struct {
	zoom, fx, fy                float64 // 放大倍数;取景框在可挪动范围里的位置(0–1,偏向中间)
	flip                        bool
	bright, contrast, sat, warm float64
	vignette                    float64
}

func lookOf(seed uint64, name string) photoLook {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	r := rand.New(rand.NewPCG(seed^h.Sum64(), seed^0x2545f4914f6cdd1d))
	u := func(lo, hi float64) float64 { return lo + (hi-lo)*r.Float64() }
	return photoLook{
		zoom: u(1, 1.18), fx: (r.Float64() + r.Float64()) / 2, fy: (r.Float64() + r.Float64()) / 2,
		flip:   r.IntN(2) == 0,
		bright: u(-.045, .045), contrast: u(.94, 1.08), sat: u(.88, 1.1), warm: u(-.04, .04),
		vignette: u(0, .12),
	}
}

// Process 把图包里的一张原图(JPEG)按种子处理成 w×h 的 JPEG。
func Process(orig []byte, seed uint64, name string, w, h int) ([]byte, error) {
	if w < 16 || h < 16 || w > 2048 || h > 2048 {
		return nil, errPhotoSize
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(orig))
	if err != nil {
		return nil, err
	}
	if cfg.Width < 64 || cfg.Height < 64 || cfg.Width > 4096 || cfg.Height > 4096 {
		return nil, errPhotoImage
	}
	img, err := jpeg.Decode(bytes.NewReader(orig))
	if err != nil {
		return nil, err
	}
	src := newPixels(img)
	lk := lookOf(seed, name)

	// 取景:先取和输出同比例的最大框,再按 zoom 缩小(缩到比输出还小就不再缩,免得放大发糊),放在可挪动范围里
	aspect := float64(w) / float64(h)
	cw, ch := float64(src.w), float64(src.w)/aspect
	if ch > float64(src.h) {
		ch, cw = float64(src.h), float64(src.h)*aspect
	}
	z := math.Min(lk.zoom, math.Max(1, math.Min(cw/float64(w), ch/float64(h))))
	cw, ch = cw/z, ch/z
	cx, cy := (float64(src.w)-cw)*lk.fx, (float64(src.h)-ch)*lk.fy

	out := &image.RGBA{Pix: make([]uint8, w*h*4), Stride: w * 4, Rect: image.Rect(0, 0, w, h)}
	cols := spans(cx, cw/float64(w), w, src.w)
	hc := &rowCache{w: w, first: int(cy), fill: func(sy int, dst []float32) {
		for ox, sp := range cols {
			var r, g, b float32
			for k, wt := range sp.wt {
				pr, pg, pb := src.at(sp.x0+k, sy)
				r, g, b = r+wt*float32(pr), g+wt*float32(pg), b+wt*float32(pb)
			}
			dst[ox*3], dst[ox*3+1], dst[ox*3+2] = r, g, b
		}
	}}
	acc := make([]float32, w*3)
	fy := ch / float64(h)
	for oy := 0; oy < h; oy++ {
		y0 := cy + float64(oy)*fy
		y1 := y0 + fy
		clear(acc)
		var total float32
		for sy := int(y0); float64(sy) < y1 && sy < src.h; sy++ {
			wt := float32(math.Min(y1, float64(sy+1)) - math.Max(y0, float64(sy)))
			if wt <= 0 {
				continue
			}
			row := hc.get(sy)
			for i, v := range row {
				acc[i] += wt * v
			}
			total += wt
		}
		// 丢到下一个输出行要用的第一行为止,和下一轮的起点用同一个算式。以前按 int(y1) 丢:y1 和下一轮的 y0 是两条
		// 浮点式子,落在整数边上时可能差一行 —— 这边丢了、那边还要,get 就下标越界(审查 F-D1)
		hc.drop(int(cy + float64(oy+1)*fy))
		if total > 0 {
			for i := range acc {
				acc[i] /= total
			}
		}
		lk.tone(acc, oy, w, h)
		for ox := 0; ox < w; ox++ {
			dx := ox
			if lk.flip {
				dx = w - 1 - ox
			}
			o := oy*out.Stride + dx*4
			out.Pix[o], out.Pix[o+1], out.Pix[o+2], out.Pix[o+3] = q8(acc[ox*3]), q8(acc[ox*3+1]), q8(acc[ox*3+2]), 255
		}
	}

	qh := fnv.New64a()
	_, _ = qh.Write([]byte(name))
	q := 74 + int((qh.Sum64()^seed^uint64(w*131+h))%13)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: q}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// tone 一行像素调色:色温、饱和度、对比度、亮度,再压一点暗角
func (lk photoLook) tone(row []float32, oy, w, h int) {
	warm, sat, con := float32(lk.warm), float32(lk.sat), float32(lk.contrast)
	br := float32(lk.bright * 255)
	dy := float64(oy)/float64(h) - .5
	for x := 0; x < w; x++ {
		r, g, b := row[x*3]*(1+warm), row[x*3+1], row[x*3+2]*(1-warm)
		l := .299*r + .587*g + .114*b
		r, g, b = l+(r-l)*sat, l+(g-l)*sat, l+(b-l)*sat
		r, g, b = (r-128)*con+128+br, (g-128)*con+128+br, (b-128)*con+128+br
		dx := float64(x)/float64(w) - .5
		k := float32(1 - lk.vignette*(dx*dx+dy*dy)*2)
		row[x*3], row[x*3+1], row[x*3+2] = r*k, g*k, b*k
	}
}

func q8(v float32) uint8 {
	switch {
	case v <= 0:
		return 0
	case v >= 255:
		return 255
	}
	return uint8(v + .5)
}

// span 输出的一列(或一行)由源图哪几个像素按什么权重合成(按面积平均)
type span struct {
	x0 int
	wt []float32
}

// spans 从 start 起每 step 个源像素合成一个输出像素,共 n 个;权重归一(超出源图的部分不算)
func spans(start, step float64, n, limit int) []span {
	out := make([]span, n)
	for i := range out {
		a := start + float64(i)*step
		b := a + step
		x0 := int(a)
		var ws []float32
		var total float32
		for x := x0; float64(x) < b && x < limit; x++ {
			wt := float32(math.Min(b, float64(x+1)) - math.Max(a, float64(x)))
			if wt < 0 {
				wt = 0
			}
			ws = append(ws, wt)
			total += wt
		}
		if total > 0 {
			for k := range ws {
				ws[k] /= total
			}
		}
		out[i] = span{x0: x0, wt: ws}
	}
	return out
}

// rowCache 横向缩好的源行:输出行往下走时按需算、用过就丢(只留当前输出行跨着的那几行)
type rowCache struct {
	w     int
	first int
	rows  [][]float32
	free  [][]float32
	fill  func(sy int, dst []float32)
}

func (c *rowCache) get(sy int) []float32 {
	if sy < c.first { // 要的是已经丢掉的行:给最早还留着的那一行,不越界(正常不会走到,见 Process 里 drop 的说明)
		sy = c.first
	}
	for c.first+len(c.rows) <= sy {
		var buf []float32
		if n := len(c.free); n > 0 {
			buf, c.free = c.free[n-1], c.free[:n-1]
		} else {
			buf = make([]float32, c.w*3)
		}
		c.fill(c.first+len(c.rows), buf)
		c.rows = append(c.rows, buf)
	}
	return c.rows[sy-c.first]
}

// drop 丢掉 before 之前的行(下一个输出行用不到了)
func (c *rowCache) drop(before int) {
	for c.first < before && len(c.rows) > 0 {
		c.free = append(c.free, c.rows[0])
		c.rows = c.rows[1:]
		c.first++
	}
	if len(c.rows) == 0 && c.first < before {
		c.first = before
	}
}

// pixels 解码后的原图,按坐标取 RGB(JPEG 解出来的 YCbCr 直接换算,不另存一份 RGB)
type pixels struct {
	w, h int
	img  image.Image
	ycc  *image.YCbCr
	gray *image.Gray
}

func newPixels(img image.Image) *pixels {
	b := img.Bounds()
	p := &pixels{w: b.Dx(), h: b.Dy(), img: img}
	switch m := img.(type) {
	case *image.YCbCr:
		p.ycc = m
	case *image.Gray:
		p.gray = m
	}
	return p
}

func (p *pixels) at(x, y int) (uint8, uint8, uint8) {
	if x < 0 {
		x = 0
	} else if x >= p.w {
		x = p.w - 1
	}
	if y < 0 {
		y = 0
	} else if y >= p.h {
		y = p.h - 1
	}
	b := p.img.Bounds()
	x, y = x+b.Min.X, y+b.Min.Y
	switch {
	case p.ycc != nil:
		yi, ci := p.ycc.YOffset(x, y), p.ycc.COffset(x, y)
		return color.YCbCrToRGB(p.ycc.Y[yi], p.ycc.Cb[ci], p.ycc.Cr[ci])
	case p.gray != nil:
		v := p.gray.GrayAt(x, y).Y
		return v, v, v
	}
	r, g, bb, _ := p.img.At(x, y).RGBA()
	return uint8(r >> 8), uint8(g >> 8), uint8(bb >> 8)
}

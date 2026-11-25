package main

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math"
)

// sparklinePNG draws a small line graph (no axes or text: the figures go in the message caption) using only the
// standard library. It draws at twice the size and averages down, which gives smooth edges without anti-aliasing code.
func sparklinePNG(vals []float64, up bool) ([]byte, error) {
	if len(vals) < 2 {
		return nil, errors.New("need at least two points")
	}
	const w, h, ss = 480, 160, 2
	const padX, padT, padB = 18, 18, 18
	W, H := w*ss, h*ss

	line := color.RGBA{0xD6, 0x3A, 0x3A, 0xFF} // red
	if up {
		line = color.RGBA{0x1F, 0x9D, 0x55, 0xFF} // green
	}
	bg := color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}
	grid := color.RGBA{0xE6, 0xE8, 0xEC, 0xFF}
	fill := blend(line, bg, 0.14)

	big := image.NewRGBA(image.Rect(0, 0, W, H))
	for i := 0; i < len(big.Pix); i += 4 {
		big.Pix[i], big.Pix[i+1], big.Pix[i+2], big.Pix[i+3] = bg.R, bg.G, bg.B, 255
	}

	lo, hi := vals[0], vals[0]
	for _, v := range vals {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	if hi-lo < 1e-9 { // a flat line sits in the middle
		lo, hi = lo-1, hi+1
	}
	span := hi - lo
	lo, hi = lo-span*0.08, hi+span*0.08
	x := func(i int) float64 {
		return float64(padX*ss) + float64(i)/float64(len(vals)-1)*float64(W-2*padX*ss)
	}
	y := func(v float64) float64 {
		return float64(padT*ss) + (1-(v-lo)/(hi-lo))*float64(H-(padT+padB)*ss)
	}

	// light horizontal guides
	for k := 0; k <= 3; k++ {
		gy := int(float64(padT*ss) + float64(k)/3*float64(H-(padT+padB)*ss))
		for gx := padX * ss; gx < W-padX*ss; gx++ {
			big.SetRGBA(gx, gy, grid)
		}
	}

	// area under the line: for each column, fill from the line's height to the baseline
	base := H - padB*ss
	for col := int(x(0)); col <= int(x(len(vals)-1)); col++ {
		fx := float64(col)
		i := 0
		for i < len(vals)-2 && fx > x(i+1) {
			i++
		}
		t := (fx - x(i)) / (x(i+1) - x(i))
		top := int(y(vals[i]) + t*(y(vals[i+1])-y(vals[i])))
		for yy := top; yy < base; yy++ {
			big.SetRGBA(col, yy, fill)
		}
	}

	for i := 0; i < len(vals)-1; i++ {
		thickLine(big, x(i), y(vals[i]), x(i+1), y(vals[i+1]), 2.6*ss/2, line)
	}
	disc(big, x(len(vals)-1), y(vals[len(vals)-1]), 5*ss, line)
	disc(big, x(len(vals)-1), y(vals[len(vals)-1]), 2.4*ss, bg)

	// average each ss×ss block
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for oy := 0; oy < h; oy++ {
		for ox := 0; ox < w; ox++ {
			var r, g, b int
			for dy := 0; dy < ss; dy++ {
				for dx := 0; dx < ss; dx++ {
					c := big.RGBAAt(ox*ss+dx, oy*ss+dy)
					r, g, b = r+int(c.R), g+int(c.G), b+int(c.B)
				}
			}
			n := ss * ss
			out.SetRGBA(ox, oy, color.RGBA{uint8(r / n), uint8(g / n), uint8(b / n), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func blend(fg, bg color.RGBA, a float64) color.RGBA {
	m := func(f, b uint8) uint8 { return uint8(float64(f)*a + float64(b)*(1-a)) }
	return color.RGBA{m(fg.R, bg.R), m(fg.G, bg.G), m(fg.B, bg.B), 255}
}

// thickLine stamps discs along a segment.
func thickLine(img *image.RGBA, x0, y0, x1, y1, r float64, c color.RGBA) {
	steps := int(math.Max(math.Abs(x1-x0), math.Abs(y1-y0))) + 1
	for s := 0; s <= steps; s++ {
		t := float64(s) / float64(steps)
		disc(img, x0+(x1-x0)*t, y0+(y1-y0)*t, r, c)
	}
}

func disc(img *image.RGBA, cx, cy, r float64, c color.RGBA) {
	b := img.Bounds()
	for yy := int(cy - r); yy <= int(cy+r); yy++ {
		for xx := int(cx - r); xx <= int(cx+r); xx++ {
			if image.Pt(xx, yy).In(b) && (float64(xx)-cx)*(float64(xx)-cx)+(float64(yy)-cy)*(float64(yy)-cy) <= r*r {
				img.SetRGBA(xx, yy, c)
			}
		}
	}
}

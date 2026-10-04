// Command genicon renders the application icon (the same design as the logo
// in internal/gui/ui/index.html) to a PNG: go run ./tools/genicon out.png
package main

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

type pt struct{ x, y float64 }

func inPoly(p pt, poly []pt) bool {
	in := false
	for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
		a, b := poly[i], poly[j]
		if (a.y > p.y) != (b.y > p.y) && p.x < (b.x-a.x)*(p.y-a.y)/(b.y-a.y)+a.x {
			in = !in
		}
	}
	return in
}

func inRoundRect(p pt, size, r float64) bool {
	cx := math.Max(r, math.Min(size-r, p.x))
	cy := math.Max(r, math.Min(size-r, p.y))
	return (p.x-cx)*(p.x-cx)+(p.y-cy)*(p.y-cy) <= r*r && p.x >= 0 && p.y >= 0 && p.x <= size && p.y <= size
}

func main() {
	const n = 256
	s := float64(n) / 64 // design is on a 64 unit grid
	sc := func(ps ...pt) []pt {
		out := make([]pt, len(ps))
		for i, p := range ps {
			out[i] = pt{p.x * s, p.y * s}
		}
		return out
	}
	top := sc(pt{32, 13}, pt{15, 22.5}, pt{32, 32}, pt{49, 22.5})
	left := sc(pt{15, 22.5}, pt{15, 41.5}, pt{32, 51}, pt{32, 32})
	right := sc(pt{49, 22.5}, pt{49, 41.5}, pt{32, 51}, pt{32, 32})
	g0 := [3]float64{0x10, 0xb9, 0x81}
	g1 := [3]float64{0x0e, 0xa5, 0xe9}
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	const ss = 4 // supersampling
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					p := pt{float64(x) + (float64(sx)+.5)/ss, float64(y) + (float64(sy)+.5)/ss}
					if !inRoundRect(p, n, 16*s) {
						continue
					}
					t := (p.x + p.y) / (2 * n)
					cr, cg, cb := g0[0]+(g1[0]-g0[0])*t, g0[1]+(g1[1]-g0[1])*t, g0[2]+(g1[2]-g0[2])*t
					white := 0.0
					switch {
					case inPoly(p, top):
						white = 1
					case inPoly(p, left):
						white = .72
					case inPoly(p, right):
						white = .48
					}
					cr += (255 - cr) * white
					cg += (255 - cg) * white
					cb += (255 - cb) * white
					r, g, b, a = r+cr, g+cg, b+cb, a+1
				}
			}
			if a > 0 {
				img.SetNRGBA(x, y, color.NRGBA{uint8(r / a), uint8(g / a), uint8(b / a), uint8(a * 255 / (ss * ss))})
			}
		}
	}
	f, err := os.Create(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer f.Close()
	png.Encode(f, img)
}

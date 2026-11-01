package server

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"strconv"
)

// The app icon, drawn here rather than shipped as files: a clock face on a
// rounded square, green while tracking and grey when stopped. The page swaps
// between the two, and the installed app's taskbar icon follows.

var (
	iconOn  = color.RGBA{0x2f, 0x8f, 0x5f, 0xff}
	iconOff = color.RGBA{0x7a, 0x7a, 0x82, 0xff}
	white   = color.RGBA{0xff, 0xff, 0xff, 0xff}
)

// coverage of a pixel by a shape, sampled 4×4 for smooth edges.
func coverage(x, y int, inside func(fx, fy float64) bool) float64 {
	n := 0
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			if inside(float64(x)+(float64(i)+0.5)/4, float64(y)+(float64(j)+0.5)/4) {
				n++
			}
		}
	}
	return float64(n) / 16
}

func blend(dst color.RGBA, src color.RGBA, a float64) color.RGBA {
	mix := func(d, s uint8) uint8 { return uint8(float64(d)*(1-a) + float64(s)*a + 0.5) }
	return color.RGBA{mix(dst.R, src.R), mix(dst.G, src.G), mix(dst.B, src.B), uint8(math.Max(float64(dst.A), a*255) + 0.5)}
}

// segment reports whether (px,py) is within w/2 of the segment a→b.
func segment(px, py, ax, ay, bx, by, w float64) bool {
	dx, dy := bx-ax, by-ay
	t := math.Max(0, math.Min(1, ((px-ax)*dx+(py-ay)*dy)/(dx*dx+dy*dy)))
	cx, cy := ax+t*dx, ay+t*dy
	return math.Hypot(px-cx, py-cy) <= w/2
}

func drawIcon(size int, on bool) []byte {
	bg := iconOff
	if on {
		bg = iconOn
	}
	s := float64(size)
	r := s * 0.22 // corner radius
	c := s / 2
	img := image.NewRGBA(image.Rect(0, 0, size, size))

	inSquare := func(x, y float64) bool {
		qx, qy := math.Max(r-x, math.Max(0, x-(s-r))), math.Max(r-y, math.Max(0, y-(s-r)))
		return qx*qx+qy*qy <= r*r
	}
	ring := func(x, y float64) bool {
		d := math.Hypot(x-c, y-c)
		return d <= s*0.33 && d >= s*0.33-s*0.075
	}
	hands := func(x, y float64) bool {
		w := s * 0.075
		return segment(x, y, c, c, c, c-s*0.2, w) || segment(x, y, c, c, c+s*0.14, c, w)
	}

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			px := color.RGBA{}
			if a := coverage(x, y, inSquare); a > 0 {
				px = blend(px, bg, a)
				if a2 := coverage(x, y, func(fx, fy float64) bool { return ring(fx, fy) || hands(fx, fy) }); a2 > 0 {
					px = blend(px, white, a2*a)
				}
			}
			img.SetRGBA(x, y, px)
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func (s *Server) icon(w http.ResponseWriter, r *http.Request) error {
	size, err := strconv.Atoi(r.URL.Query().Get("size"))
	if err != nil || size < 16 || size > 512 {
		size = 192
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, err = w.Write(drawIcon(size, r.URL.Query().Get("on") == "1"))
	return err
}

const manifest = `{
  "name": "timew",
  "short_name": "timew",
  "start_url": "/",
  "scope": "/",
  "display": "standalone",
  "background_color": "#151517",
  "theme_color": "#2f6f4f",
  "icons": [
    {"src": "/icon.png?size=192", "sizes": "192x192", "type": "image/png"},
    {"src": "/icon.png?size=512", "sizes": "512x512", "type": "image/png"},
    {"src": "/icon.png?size=512", "sizes": "512x512", "type": "image/png", "purpose": "maskable"}
  ]
}
`

func (s *Server) manifest(w http.ResponseWriter, _ *http.Request) error {
	w.Header().Set("Content-Type", "application/manifest+json")
	_, err := w.Write([]byte(manifest))
	return err
}

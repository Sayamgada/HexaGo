// Command icongen draws the HexaGo app icon (an amber hexagon ring with a hex
// core on the night-navy brand background) and writes every platform asset of
// the Flutter app. Standard library only:
//
//	go run tools/icongen/main.go            (from the repository root)
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
)

var (
	navy  = color.NRGBA{0x07, 0x0B, 0x14, 0xFF} // HexaColors.background
	amber = color.NRGBA{0xFF, 0xB0, 0x20, 0xFF} // HexaColors.amber
	clear = color.NRGBA{}
)

// style controls how much of the canvas the hexagon fills and the backdrop.
type style struct {
	background  bool    // paint the navy backdrop
	rounded     float64 // backdrop corner radius as a fraction of the size (0 = full bleed)
	hexFraction float64 // hexagon circumradius as a fraction of the size
}

var (
	appIcon   = style{background: true, rounded: 0.22, hexFraction: 0.40}
	maskable  = style{background: true, rounded: 0, hexFraction: 0.30} // inside the 80 % safe zone
	adaptiveF = style{background: false, hexFraction: 0.27}           // 108 dp canvas, 72 dp safe zone
)

// hexContains reports whether (x, y) is inside the pointy-top hexagon of
// circumradius r centred at (cx, cy).
func hexContains(x, y, cx, cy, r float64) bool {
	dx, dy := math.Abs(x-cx), math.Abs(y-cy)
	h := r * math.Sqrt(3) / 2 // half width
	if dx > h || dy > r {
		return false
	}
	return dy <= r-dx/math.Sqrt(3)
}

func roundedContains(x, y, size, radius float64) bool {
	if radius <= 0 {
		return true
	}
	cx := math.Max(radius, math.Min(x, size-radius))
	cy := math.Max(radius, math.Min(y, size-radius))
	return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= radius*radius
}

// shade returns the colour of one sample point.
func shade(x, y, size float64, s style) (color.NRGBA, bool) {
	c := size / 2
	r := s.hexFraction * size
	switch {
	case hexContains(x, y, c, c, r*0.42): // core
		return amber, true
	case hexContains(x, y, c, c, r*0.66): // gap
		if s.background {
			return navy, true
		}
		return clear, false
	case hexContains(x, y, c, c, r): // ring
		return amber, true
	}
	if s.background && roundedContains(x, y, size, s.rounded*size) {
		return navy, true
	}
	return clear, false
}

// render draws the icon at n x n pixels with 4x4 supersampling.
func render(n int, s style) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	const ss = 4
	size := float64(n)
	for py := 0; py < n; py++ {
		for px := 0; px < n; px++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := float64(px) + (float64(sx)+0.5)/ss
					y := float64(py) + (float64(sy)+0.5)/ss
					if col, ok := shade(x, y, size, s); ok {
						r += float64(col.R)
						g += float64(col.G)
						b += float64(col.B)
						a++
					}
				}
			}
			if a == 0 {
				continue
			}
			img.SetNRGBA(px, py, color.NRGBA{uint8(r / a), uint8(g / a), uint8(b / a), uint8(255 * a / (ss * ss))})
		}
	}
	return img
}

func encode(img image.Image) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		log.Fatal(err)
	}
	return buf.Bytes()
}

func write(path string, data []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Println("wrote", path)
}

// ico packs PNG images into a Windows .ico (PNG entries, Vista and later).
func ico(sizes []int, s style) []byte {
	var head, body bytes.Buffer
	_ = binary.Write(&head, binary.LittleEndian, [3]uint16{0, 1, uint16(len(sizes))})
	offset := 6 + 16*len(sizes)
	for _, n := range sizes {
		data := encode(render(n, s))
		dim := uint8(n)
		if n >= 256 {
			dim = 0
		}
		_ = binary.Write(&head, binary.LittleEndian, struct {
			W, H, Colors, Reserved uint8
			Planes, BPP            uint16
			Size, Offset           uint32
		}{dim, dim, 0, 0, 1, 32, uint32(len(data)), uint32(offset)})
		body.Write(data)
		offset += len(data)
	}
	return append(head.Bytes(), body.Bytes()...)
}

func main() {
	app := filepath.Join("apps", "hexago_app")
	if _, err := os.Stat(app); err != nil {
		log.Fatalf("run from the repository root: %v", err)
	}

	// Windows
	write(filepath.Join(app, "windows", "runner", "resources", "app_icon.ico"), ico([]int{16, 24, 32, 48, 64, 128, 256}, appIcon))

	// Web favicon and PWA icons
	web := filepath.Join(app, "web")
	write(filepath.Join(web, "favicon.png"), encode(render(64, appIcon)))
	write(filepath.Join(web, "icons", "Icon-192.png"), encode(render(192, appIcon)))
	write(filepath.Join(web, "icons", "Icon-512.png"), encode(render(512, appIcon)))
	write(filepath.Join(web, "icons", "Icon-maskable-192.png"), encode(render(192, maskable)))
	write(filepath.Join(web, "icons", "Icon-maskable-512.png"), encode(render(512, maskable)))

	// Android: legacy launcher icons and adaptive-icon foregrounds.
	res := filepath.Join(app, "android", "app", "src", "main", "res")
	for density, legacy := range map[string]int{"mdpi": 48, "hdpi": 72, "xhdpi": 96, "xxhdpi": 144, "xxxhdpi": 192} {
		write(filepath.Join(res, "mipmap-"+density, "ic_launcher.png"), encode(render(legacy, appIcon)))
		write(filepath.Join(res, "mipmap-"+density, "ic_launcher_foreground.png"), encode(render(legacy*108/48, adaptiveF)))
	}
	write(filepath.Join(res, "mipmap-anydpi-v26", "ic_launcher.xml"), []byte(`<?xml version="1.0" encoding="utf-8"?>
<adaptive-icon xmlns:android="http://schemas.android.com/apk/res/android">
    <background android:drawable="@color/ic_launcher_background"/>
    <foreground android:drawable="@mipmap/ic_launcher_foreground"/>
    <monochrome android:drawable="@mipmap/ic_launcher_foreground"/>
</adaptive-icon>
`))
	write(filepath.Join(res, "values", "ic_launcher_background.xml"), []byte(`<?xml version="1.0" encoding="utf-8"?>
<resources>
    <color name="ic_launcher_background">#070B14</color>
</resources>
`))
}

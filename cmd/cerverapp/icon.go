//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
)

// icon draws the tray icon (a ring, colored by state) as an .ico holding
// one 32×32 PNG, which Windows reads as is.
func icon(state string) []byte {
	c := map[string]color.NRGBA{
		"ready":   {34, 197, 94, 255},
		"working": {245, 158, 11, 255},
	}[state]
	if c.A == 0 {
		c = color.NRGBA{148, 163, 184, 255}
	}
	const n = 32
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			d := math.Hypot(float64(x)-15.5, float64(y)-15.5)
			// ring from radius 8.5 to 14.5, soft edges; open on the right like a "C"
			a := clamp(14.5-d+0.5) * clamp(d-8.5+0.5)
			if x > 20 && math.Abs(float64(y)-15.5) < 4 {
				a = 0
			}
			if a > 0 {
				img.SetNRGBA(x, y, color.NRGBA{c.R, c.G, c.B, uint8(a * 255)})
			}
		}
	}
	var p bytes.Buffer
	png.Encode(&p, img)
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, []uint16{0, 1, 1})                 // ICONDIR
	b.Write([]byte{n, n, 0, 0})                                              // width, height, colors, reserved
	binary.Write(&b, binary.LittleEndian, []uint16{1, 32})                   // planes, bit count
	binary.Write(&b, binary.LittleEndian, []uint32{uint32(p.Len()), 6 + 16}) // size, offset
	b.Write(p.Bytes())
	return b.Bytes()
}

func clamp(v float64) float64 { return math.Max(0, math.Min(1, v)) }

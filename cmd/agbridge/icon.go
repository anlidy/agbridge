package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
)

// trayIcon draws the tray icon (a bridge arc on a blue disc) as an ICO.
func trayIcon() []byte {
	const n = 32
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	c := float64(n-1) / 2
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			dx, dy := float64(x)-c, float64(y)-c
			d := math.Hypot(dx, dy)
			if d > c+0.5 {
				continue
			}
			alpha := uint8(255)
			if d > c-0.5 {
				alpha = uint8(255 * (c + 0.5 - d))
			}
			col := color.RGBA{37, 99, 235, alpha}
			// Arc of the bridge.
			ad := math.Hypot(dx, dy-6)
			if ad > 9 && ad < 12.5 && dy-6 < 0 {
				col = color.RGBA{255, 255, 255, alpha}
			}
			// Deck.
			if dy > 5 && dy < 8.5 && math.Abs(dx) < 12 {
				col = color.RGBA{255, 255, 255, alpha}
			}
			img.SetRGBA(x, y, col)
		}
	}
	var pngBuf bytes.Buffer
	_ = png.Encode(&pngBuf, img)
	var ico bytes.Buffer
	_ = binary.Write(&ico, binary.LittleEndian, [3]uint16{0, 1, 1})
	_ = binary.Write(&ico, binary.LittleEndian, struct {
		W, H, Colors, Reserved uint8
		Planes, BPP            uint16
		Size, Offset           uint32
	}{n, n, 0, 0, 1, 32, uint32(pngBuf.Len()), 22})
	ico.Write(pngBuf.Bytes())
	return ico.Bytes()
}

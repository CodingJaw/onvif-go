package render

import (
	"image"
	"image/color"
	"image/draw"

	"github.com/0x524a/onvif-go/apps/rtsp-presence-server/internal/presence"
)

var (
	bgColor      = color.RGBA{R: 12, G: 15, B: 24, A: 255}
	gridColor    = color.RGBA{R: 48, G: 58, B: 77, A: 255}
	wifiColor    = color.RGBA{R: 47, G: 191, B: 113, A: 255}
	bluetoothCol = color.RGBA{R: 76, G: 153, B: 255, A: 255}
)

func PresenceChart(width, height int, view presence.View, samples []presence.Sample) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: bgColor}, image.Point{}, draw.Src)

	const pad = 24
	plot := image.Rect(pad, pad, width-pad, height-pad)

	for i := 0; i <= 4; i++ {
		y := plot.Min.Y + i*(plot.Dy())/4
		for x := plot.Min.X; x < plot.Max.X; x++ {
			img.Set(x, y, gridColor)
		}
	}

	maxV := 1
	for _, s := range samples {
		if s.WiFiCount > maxV {
			maxV = s.WiFiCount
		}
		if s.BluetoothCount > maxV {
			maxV = s.BluetoothCount
		}
	}

	if len(samples) < 2 {
		return img
	}

	drawSeries := func(use func(presence.Sample) int, col color.Color) {
		for i := 1; i < len(samples); i++ {
			x0 := plot.Min.X + (i-1)*plot.Dx()/(len(samples)-1)
			x1 := plot.Min.X + i*plot.Dx()/(len(samples)-1)
			y0 := plot.Max.Y - (use(samples[i-1])*plot.Dy())/maxV
			y1 := plot.Max.Y - (use(samples[i])*plot.Dy())/maxV
			drawLine(img, x0, y0, x1, y1, col)
		}
	}

	if view.Source == presence.SourceBoth || view.Source == presence.SourceWiFi {
		drawSeries(func(s presence.Sample) int { return s.WiFiCount }, wifiColor)
	}
	if view.Source == presence.SourceBoth || view.Source == presence.SourceBluetooth {
		drawSeries(func(s presence.Sample) int { return s.BluetoothCount }, bluetoothCol)
	}

	return img
}

func drawLine(img *image.RGBA, x0, y0, x1, y1 int, col color.Color) {
	dx := abs(x1 - x0)
	sx := -1
	if x0 < x1 {
		sx = 1
	}
	dy := -abs(y1 - y0)
	sy := -1
	if y0 < y1 {
		sy = 1
	}
	err := dx + dy

	for {
		img.Set(x0, y0, col)
		if x0 == x1 && y0 == y1 {
			break
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x0 += sx
		}
		if e2 <= dx {
			err += dx
			y0 += sy
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

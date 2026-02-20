package render

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"time"

	"github.com/0x524a/onvif-go/apps/rtsp-presence-server/internal/presence"
)

var (
	bgColor       = color.RGBA{R: 12, G: 15, B: 24, A: 255}
	gridColor     = color.RGBA{R: 48, G: 58, B: 77, A: 255}
	wifiColor     = color.RGBA{R: 47, G: 191, B: 113, A: 255}
	bluetoothCol  = color.RGBA{R: 76, G: 153, B: 255, A: 255}
	axisColor     = color.RGBA{R: 180, G: 190, B: 210, A: 255}
	labelColor    = color.RGBA{R: 220, G: 230, B: 250, A: 255}
	secondaryText = color.RGBA{R: 140, G: 150, B: 170, A: 255}
)

func PresenceChart(width, height int, now time.Time, view presence.View, samples []presence.Sample) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: bgColor}, image.Point{}, draw.Src)

	plot := image.Rect(58, 20, width-20, height-46)
	drawAxes(img, plot)
	drawGrid(img, plot)

	windowDur := windowToDuration(view.Window)
	windowStart := now.Add(-windowDur)

	maxV := 1
	for _, s := range samples {
		if s.WiFiCount > maxV {
			maxV = s.WiFiCount
		}
		if s.BluetoothCount > maxV {
			maxV = s.BluetoothCount
		}
	}

	drawYLabels(img, plot, maxV)
	drawXLabels(img, plot, view.Window)
	drawLegend(img, view)

	if len(samples) == 0 {
		drawString(img, plot.Min.X+6, plot.Min.Y+6, "waiting for data", secondaryText)
		return img
	}

	drawSeries := func(use func(presence.Sample) int, col color.Color) {
		prev, hasPrev := pointForSample(plot, windowStart, now, maxV, use(samples[0]), samples[0].Timestamp)
		if !hasPrev {
			return
		}
		for i := 1; i < len(samples); i++ {
			cur, ok := pointForSample(plot, windowStart, now, maxV, use(samples[i]), samples[i].Timestamp)
			if !ok {
				continue
			}
			drawLine(img, prev.X, prev.Y, cur.X, cur.Y, col)
			prev = cur
		}
	}

	switch view.DisplayMode {
	case presence.DisplayModeBar:
		drawBars(img, plot, windowStart, now, maxV, view, samples)
	case presence.DisplayModeHistogram:
		drawHistogram(img, plot, maxV, view, samples)
	case presence.DisplayModeText:
		drawTextMode(img, plot, view, samples)
	default:
		if view.Source == presence.SourceBoth || view.Source == presence.SourceWiFi {
			drawSeries(func(s presence.Sample) int { return s.WiFiCount }, wifiColor)
		}
		if view.Source == presence.SourceBoth || view.Source == presence.SourceBluetooth {
			drawSeries(func(s presence.Sample) int { return s.BluetoothCount }, bluetoothCol)
		}
	}

	return img
}

func pointForSample(plot image.Rectangle, start, now time.Time, maxV, value int, ts time.Time) (image.Point, bool) {
	if ts.Before(start) || ts.After(now) {
		return image.Point{}, false
	}
	total := now.Sub(start)
	if total <= 0 {
		return image.Point{X: plot.Max.X, Y: plot.Max.Y}, true
	}
	elapsed := ts.Sub(start)
	x := plot.Min.X + int((int64(elapsed)*int64(plot.Dx()))/int64(total))
	y := plot.Max.Y - (value*plot.Dy())/maxV
	if y < plot.Min.Y {
		y = plot.Min.Y
	}
	if y > plot.Max.Y {
		y = plot.Max.Y
	}
	if x < plot.Min.X {
		x = plot.Min.X
	}
	if x > plot.Max.X {
		x = plot.Max.X
	}
	return image.Point{X: x, Y: y}, true
}

func drawAxes(img *image.RGBA, plot image.Rectangle) {
	for x := plot.Min.X; x <= plot.Max.X; x++ {
		img.Set(x, plot.Max.Y, axisColor)
	}
	for y := plot.Min.Y; y <= plot.Max.Y; y++ {
		img.Set(plot.Min.X, y, axisColor)
	}
}

func drawGrid(img *image.RGBA, plot image.Rectangle) {
	for i := 1; i <= 4; i++ {
		y := plot.Min.Y + i*(plot.Dy())/5
		for x := plot.Min.X; x <= plot.Max.X; x++ {
			img.Set(x, y, gridColor)
		}
	}
	for i := 1; i <= 4; i++ {
		x := plot.Min.X + i*(plot.Dx())/5
		for y := plot.Min.Y; y <= plot.Max.Y; y++ {
			img.Set(x, y, gridColor)
		}
	}
}

func drawYLabels(img *image.RGBA, plot image.Rectangle, maxV int) {
	for i := 0; i <= 5; i++ {
		v := (maxV * (5 - i)) / 5
		y := plot.Min.Y + i*(plot.Dy())/5 - 3
		drawString(img, 6, y, fmt.Sprintf("%d", v), labelColor)
	}
	drawString(img, 6, plot.Min.Y-10, "count", secondaryText)
}

func drawXLabels(img *image.RGBA, plot image.Rectangle, window presence.Window) {
	d := windowToDuration(window)
	for i := 0; i <= 5; i++ {
		x := plot.Min.X + i*(plot.Dx())/5 - 8
		age := time.Duration((int64(d) * int64(5-i)) / 5)
		label := "now"
		if i < 5 {
			label = "-" + shortDur(age)
		}
		drawString(img, x, plot.Max.Y+8, label, labelColor)
	}
}

func drawLegend(img *image.RGBA, view presence.View) {
	drawString(img, 70, 4, "window:"+string(view.Window), secondaryText)
	drawString(img, 220, 4, "source:"+string(view.Source), secondaryText)
	drawString(img, 360, 4, "mode:"+string(view.DisplayMode), secondaryText)
	drawString(img, 520, 4, "wifi", wifiColor)
	drawString(img, 560, 4, "bluetooth", bluetoothCol)
}

func windowToDuration(w presence.Window) time.Duration {
	switch w {
	case presence.Window10M:
		return 10 * time.Minute
	case presence.Window1H:
		return time.Hour
	case presence.Window6H:
		return 6 * time.Hour
	case presence.Window12H:
		return 12 * time.Hour
	case presence.Window24H:
		return 24 * time.Hour
	default:
		return 2 * time.Minute
	}
}

func shortDur(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

func drawBars(
	img *image.RGBA,
	plot image.Rectangle,
	windowStart, now time.Time,
	maxV int,
	view presence.View,
	samples []presence.Sample,
) {
	bars := 20
	if plot.Dx() < 400 {
		bars = 12
	}
	if bars <= 0 {
		return
	}
	type agg struct{ wifi, bt, c int }
	aggs := make([]agg, bars)
	dur := now.Sub(windowStart)
	if dur <= 0 {
		return
	}
	for _, s := range samples {
		if s.Timestamp.Before(windowStart) || s.Timestamp.After(now) {
			continue
		}
		idx := int((int64(s.Timestamp.Sub(windowStart)) * int64(bars)) / int64(dur))
		if idx < 0 {
			idx = 0
		}
		if idx >= bars {
			idx = bars - 1
		}
		aggs[idx].wifi += s.WiFiCount
		aggs[idx].bt += s.BluetoothCount
		aggs[idx].c++
	}
	barW := max(2, plot.Dx()/bars)
	for i, a := range aggs {
		if a.c == 0 {
			continue
		}
		x0 := plot.Min.X + i*plot.Dx()/bars
		x1 := min(plot.Max.X, x0+barW-1)
		wifiAvg := a.wifi / a.c
		btAvg := a.bt / a.c
		if view.Source == presence.SourceBoth || view.Source == presence.SourceWiFi {
			y := plot.Max.Y - (wifiAvg*plot.Dy())/maxV
			fillRect(img, image.Rect(x0, y, x1, plot.Max.Y), wifiColor)
		}
		if view.Source == presence.SourceBoth || view.Source == presence.SourceBluetooth {
			y := plot.Max.Y - (btAvg*plot.Dy())/maxV
			fillRect(img, image.Rect(x0+barW/3, y, x1, plot.Max.Y), bluetoothCol)
		}
	}
}

func drawHistogram(img *image.RGBA, plot image.Rectangle, maxV int, view presence.View, samples []presence.Sample) {
	buckets := 10
	if maxV < buckets {
		buckets = maxV
	}
	if buckets <= 0 {
		return
	}
	wifiHist := make([]int, buckets)
	btHist := make([]int, buckets)
	for _, s := range samples {
		wi := min(buckets-1, (s.WiFiCount*buckets)/max(1, maxV+1))
		bi := min(buckets-1, (s.BluetoothCount*buckets)/max(1, maxV+1))
		wifiHist[wi]++
		btHist[bi]++
	}
	maxCount := 1
	for i := 0; i < buckets; i++ {
		if wifiHist[i] > maxCount {
			maxCount = wifiHist[i]
		}
		if btHist[i] > maxCount {
			maxCount = btHist[i]
		}
	}
	colW := max(2, plot.Dx()/buckets)
	for i := 0; i < buckets; i++ {
		x0 := plot.Min.X + i*plot.Dx()/buckets
		x1 := min(plot.Max.X, x0+colW-1)
		if view.Source == presence.SourceBoth || view.Source == presence.SourceWiFi {
			y := plot.Max.Y - (wifiHist[i]*plot.Dy())/maxCount
			fillRect(img, image.Rect(x0, y, x1, plot.Max.Y), wifiColor)
		}
		if view.Source == presence.SourceBoth || view.Source == presence.SourceBluetooth {
			y := plot.Max.Y - (btHist[i]*plot.Dy())/maxCount
			fillRect(img, image.Rect(x0+colW/3, y, x1, plot.Max.Y), bluetoothCol)
		}
	}
}

func fillRect(img *image.RGBA, r image.Rectangle, col color.Color) {
	if r.Min.X > r.Max.X || r.Min.Y > r.Max.Y {
		return
	}
	if r.Min.X < img.Bounds().Min.X {
		r.Min.X = img.Bounds().Min.X
	}
	if r.Max.X > img.Bounds().Max.X {
		r.Max.X = img.Bounds().Max.X
	}
	if r.Min.Y < img.Bounds().Min.Y {
		r.Min.Y = img.Bounds().Min.Y
	}
	if r.Max.Y > img.Bounds().Max.Y {
		r.Max.Y = img.Bounds().Max.Y
	}
	for y := r.Min.Y; y <= r.Max.Y; y++ {
		for x := r.Min.X; x <= r.Max.X; x++ {
			img.Set(x, y, col)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func drawTextMode(img *image.RGBA, plot image.Rectangle, view presence.View, samples []presence.Sample) {
	latest := samples[len(samples)-1]

	halfW := plot.Dx() / 2
	leftCenterX := plot.Min.X + halfW/2
	rightCenterX := plot.Min.X + halfW + halfW/2
	centerY := plot.Min.Y + plot.Dy()/2
	labelScale := 12
	numberScale := 32
	labelY := plot.Min.Y + 36

	if view.Source == presence.SourceBoth || view.Source == presence.SourceWiFi {
		wifiLabel := "wifi"
		wifiValue := fmt.Sprintf("%d", latest.WiFiCount)
		wifiValueW := stringWidthScaled(wifiValue, numberScale)
		wifiValueH := stringHeightScaled(numberScale)
		wifiValueX := leftCenterX - wifiValueW/2
		wifiValueY := centerY - wifiValueH/2
		drawCenteredStringScaled(img, leftCenterX, labelY, wifiLabel, wifiColor, labelScale)
		drawStringScaled(img, wifiValueX, wifiValueY, wifiValue, labelColor, numberScale)
	}
	if view.Source == presence.SourceBoth || view.Source == presence.SourceBluetooth {
		btLabel := "bluetooth"
		btValue := fmt.Sprintf("%d", latest.BluetoothCount)
		btValueW := stringWidthScaled(btValue, numberScale)
		btValueH := stringHeightScaled(numberScale)
		btValueX := rightCenterX - btValueW/2
		btValueY := centerY - btValueH/2
		drawCenteredStringScaled(img, rightCenterX, labelY, btLabel, bluetoothCol, labelScale)
		drawStringScaled(img, btValueX, btValueY, btValue, labelColor, numberScale)
	}
}

func drawCenteredStringScaled(img *image.RGBA, centerX, y int, text string, col color.Color, scale int) {
	w := stringWidthScaled(text, scale)
	drawStringScaled(img, centerX-w/2, y, text, col, scale)
}

func stringWidthScaled(text string, scale int) int {
	if scale < 1 {
		scale = 1
	}
	return len(text) * 4 * scale
}

func stringHeightScaled(scale int) int {
	if scale < 1 {
		scale = 1
	}
	return 5 * scale
}

func drawStringScaled(img *image.RGBA, x, y int, text string, col color.Color, scale int) {
	if scale < 1 {
		scale = 1
	}
	cx := x
	for _, r := range text {
		g, ok := glyphs[r]
		if !ok && r >= 'A' && r <= 'Z' {
			g, ok = glyphs[r+('a'-'A')]
		}
		if !ok {
			cx += 4 * scale
			continue
		}
		for gy, row := range g {
			for gx, ch := range row {
				if ch != '1' {
					continue
				}
				for sy := 0; sy < scale; sy++ {
					for sx := 0; sx < scale; sx++ {
						px := cx + gx*scale + sx
						py := y + gy*scale + sy
						if image.Pt(px, py).In(img.Bounds()) {
							img.Set(px, py, col)
						}
					}
				}
			}
		}
		cx += 4 * scale
	}
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

var glyphs = map[rune][]string{
	'0': {"111", "101", "101", "101", "111"},
	'1': {"010", "110", "010", "010", "111"},
	'2': {"111", "001", "111", "100", "111"},
	'3': {"111", "001", "111", "001", "111"},
	'4': {"101", "101", "111", "001", "001"},
	'5': {"111", "100", "111", "001", "111"},
	'6': {"111", "100", "111", "101", "111"},
	'7': {"111", "001", "001", "001", "001"},
	'8': {"111", "101", "111", "101", "111"},
	'9': {"111", "101", "111", "001", "111"},
	'-': {"000", "000", "111", "000", "000"},
	':': {"000", "010", "000", "010", "000"},
	' ': {"000", "000", "000", "000", "000"},
	'a': {"000", "110", "001", "111", "111"},
	'b': {"100", "100", "110", "101", "110"},
	'c': {"000", "011", "100", "100", "011"},
	'd': {"001", "001", "011", "101", "011"},
	'e': {"010", "101", "111", "100", "011"},
	'f': {"011", "100", "110", "100", "100"},
	'h': {"100", "100", "111", "101", "101"},
	'i': {"010", "000", "010", "010", "010"},
	'l': {"100", "100", "100", "100", "011"},
	'n': {"000", "110", "101", "101", "101"},
	'o': {"000", "111", "101", "101", "111"},
	'r': {"000", "110", "101", "100", "100"},
	's': {"011", "100", "010", "001", "110"},
	't': {"010", "111", "010", "010", "011"},
	'u': {"000", "101", "101", "101", "111"},
	'w': {"000", "101", "101", "111", "010"},
}

func drawString(img *image.RGBA, x, y int, text string, col color.Color) {
	cx := x
	for _, r := range text {
		g, ok := glyphs[r]
		if !ok {
			if r >= 'A' && r <= 'Z' {
				g, ok = glyphs[r+('a'-'A')]
			}
		}
		if !ok {
			cx += 4
			continue
		}
		for gy, row := range g {
			for gx, ch := range row {
				if ch == '1' {
					px := cx + gx
					py := y + gy
					if image.Pt(px, py).In(img.Bounds()) {
						img.Set(px, py, col)
					}
				}
			}
		}
		cx += 4
	}
}

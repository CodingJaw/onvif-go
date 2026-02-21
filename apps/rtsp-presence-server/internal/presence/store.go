package presence

import (
	"errors"
	"sync"
	"time"
)

// Source controls which presence counters should be rendered.
type Source string

const (
	SourceBoth      Source = "both"
	SourceWiFi      Source = "wifi"
	SourceBluetooth Source = "bluetooth"
)

// Window controls the historical range to plot.
type Window string

const (
	WindowLive Window = "live"
	Window10M  Window = "10m"
	Window1H   Window = "1h"
	Window6H   Window = "6h"
	Window12H  Window = "12h"
	Window24H  Window = "24h"
)

// Sample is one observation from the python collector.
type Sample struct {
	Timestamp      time.Time `json:"timestamp"`
	WiFiCount      int       `json:"wifi_count"`
	BluetoothCount int       `json:"bluetooth_count"`
}

// View is the active rendering selection.
type View struct {
	Window      Window      `json:"window"`
	Source      Source      `json:"source"`
	DisplayMode DisplayMode `json:"display_mode,omitempty"`
}

// DisplayMode controls how data is drawn.
type DisplayMode string

const (
	DisplayModeLine      DisplayMode = "line"
	DisplayModeBar       DisplayMode = "bar"
	DisplayModeHistogram DisplayMode = "histogram"
	DisplayModeText      DisplayMode = "text"
)

// Store stores incoming samples and current view selection.
type Store struct {
	mu      sync.RWMutex
	samples []Sample
	view    View
}

func NewStore() *Store {
	return &Store{view: View{Window: WindowLive, Source: SourceBoth, DisplayMode: DisplayModeLine}}
}

func (s *Store) Add(sample Sample) error {
	if sample.Timestamp.IsZero() {
		sample.Timestamp = time.Now().UTC()
	}
	if sample.WiFiCount < 0 || sample.BluetoothCount < 0 {
		return errors.New("counts must be >= 0")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.samples = append(s.samples, sample)
	cutoff := time.Now().Add(-24 * time.Hour)
	idx := 0
	for idx < len(s.samples) && s.samples[idx].Timestamp.Before(cutoff) {
		idx++
	}
	if idx > 0 {
		s.samples = append([]Sample(nil), s.samples[idx:]...)
	}

	return nil
}

func (s *Store) SetView(v View) error {
	if v.DisplayMode == "" {
		v.DisplayMode = DisplayModeLine
	}
	if !validWindow(v.Window) {
		return errors.New("invalid window")
	}
	if !validSource(v.Source) {
		return errors.New("invalid source")
	}
	if !validDisplayMode(v.DisplayMode) {
		return errors.New("invalid display_mode")
	}
	s.mu.Lock()
	s.view = v
	s.mu.Unlock()
	return nil
}

func (s *Store) View() View {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.view
}

func (s *Store) Snapshot(now time.Time) (View, []Sample) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	view := s.view
	start := now.Add(-windowDuration(view.Window))
	if view.Window == WindowLive {
		start = now.Add(-2 * time.Minute)
	}

	out := make([]Sample, 0, len(s.samples))
	for _, sample := range s.samples {
		if sample.Timestamp.After(start) || sample.Timestamp.Equal(start) {
			out = append(out, sample)
		}
	}
	return view, out
}

func validSource(s Source) bool {
	switch s {
	case SourceBoth, SourceWiFi, SourceBluetooth:
		return true
	default:
		return false
	}
}

func validWindow(w Window) bool {
	switch w {
	case WindowLive, Window10M, Window1H, Window6H, Window12H, Window24H:
		return true
	default:
		return false
	}
}

func validDisplayMode(m DisplayMode) bool {
	switch m {
	case DisplayModeLine, DisplayModeBar, DisplayModeHistogram, DisplayModeText:
		return true
	default:
		return false
	}
}

func windowDuration(w Window) time.Duration {
	switch w {
	case Window10M:
		return 10 * time.Minute
	case Window1H:
		return time.Hour
	case Window6H:
		return 6 * time.Hour
	case Window12H:
		return 12 * time.Hour
	case Window24H:
		return 24 * time.Hour
	default:
		return 2 * time.Minute
	}
}

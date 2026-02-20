package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"image/jpeg"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtpmjpeg"

	"github.com/0x524a/onvif-go/apps/rtsp-presence-server/internal/presence"
	"github.com/0x524a/onvif-go/apps/rtsp-presence-server/internal/render"
)

type rtspHandler struct {
	stream *gortsplib.ServerStream
	debug  bool
	mu     sync.RWMutex
}

func (h *rtspHandler) debugf(format string, args ...interface{}) {
	if h.debug {
		log.Printf("[debug] "+format, args...)
	}
}

func (h *rtspHandler) OnConnOpen(_ *gortsplib.ServerHandlerOnConnOpenCtx) {
	h.debugf("RTSP connection opened")
}

func (h *rtspHandler) OnConnClose(ctx *gortsplib.ServerHandlerOnConnCloseCtx) {
	h.debugf("RTSP connection closed: %v", ctx.Error)
}

func (h *rtspHandler) OnSessionOpen(_ *gortsplib.ServerHandlerOnSessionOpenCtx) {
	h.debugf("RTSP session opened")
}

func (h *rtspHandler) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	h.debugf("RTSP session closed: %v", ctx.Error)
}

func (h *rtspHandler) OnDescribe(_ *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	h.debugf("DESCRIBE")
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.stream == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, h.stream, nil
}

func (h *rtspHandler) OnSetup(_ *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	h.debugf("SETUP")
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.stream == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, h.stream, nil
}

func (h *rtspHandler) OnPlay(_ *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	h.debugf("PLAY")
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func main() {
	var (
		rtspAddr = flag.String("rtsp-addr", ":8554", "RTSP bind address")
		httpAddr = flag.String("http-addr", ":18080", "HTTP API bind address")
		path     = flag.String("path", "presence", "RTSP path")
		fps      = flag.Int("fps", 5, "stream FPS")
		width    = flag.Int("width", 1280, "video width")
		height   = flag.Int("height", 720, "video height")
		host     = flag.String("host", "127.0.0.1", "host/IP for displayed stream/API URLs")
		debug    = flag.Bool("debug", false, "enable verbose debug logging")
	)
	flag.Parse()
	*path = normalizePath(*path)

	store := presence.NewStore()

	h := &rtspHandler{debug: *debug}
	rtspServer := &gortsplib.Server{
		Handler:           h,
		RTSPAddress:       *rtspAddr,
		UDPRTPAddress:     ":8000",
		UDPRTCPAddress:    ":8001",
		MulticastIPRange:  "224.1.0.0/16",
		MulticastRTPPort:  8002,
		MulticastRTCPPort: 8003,
	}
	if err := rtspServer.Start(); err != nil {
		log.Fatalf("failed to start RTSP server: %v", err)
	}
	defer rtspServer.Close()

	mjpegFmt := &format.MJPEG{}
	desc := &description.Session{Medias: []*description.Media{{
		Type:    description.MediaTypeVideo,
		Control: *path,
		Formats: []format.Format{mjpegFmt},
	}}}
	stream := &gortsplib.ServerStream{Server: rtspServer, Desc: desc}
	if err := stream.Initialize(); err != nil {
		log.Fatalf("failed to initialize stream: %v", err)
	}
	defer stream.Close()
	h.stream = stream

	rtpEnc, err := mjpegFmt.CreateEncoder()
	if err != nil {
		log.Fatalf("failed to create MJPEG RTP encoder: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go serveHTTPAPI(ctx, *httpAddr, store, *debug)
	go streamLoopMJPEG(ctx, stream, desc.Medias[0], rtpEnc, store, *fps, *width, *height, *debug)

	log.Printf("RTSP stream ready (gortsplib MJPEG): rtsp://%s%s/%s", *host, *rtspAddr, *path)
	log.Printf("HTTP control API: http://%s%s/api/v1", *host, *httpAddr)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	cancel()
	time.Sleep(300 * time.Millisecond)
}

func normalizePath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return "presence"
	}
	return path
}

func streamLoopMJPEG(
	ctx context.Context,
	stream *gortsplib.ServerStream,
	media *description.Media,
	rtpEnc *rtpmjpeg.Encoder,
	store *presence.Store,
	fps, width, height int,
	debug bool,
) {
	if fps <= 0 {
		fps = 5
	}

	clockRate := uint32(90000)
	ticksPerFrame := clockRate / uint32(fps)
	if ticksPerFrame == 0 {
		ticksPerFrame = 1
	}
	rtpTimestamp := randomUint32()

	ticker := time.NewTicker(time.Second / time.Duration(fps))
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			jpegData, ok := renderFrameJPEG(now, store, width, height)
			if !ok {
				continue
			}

			pkts, err := rtpEnc.Encode(jpegData)
			if err != nil {
				log.Printf("rtp encode failed: %v", err)
				continue
			}

			if debug {
				log.Printf("[debug] mjpeg frame bytes=%d pkts=%d", len(jpegData), len(pkts))
			}

			for _, pkt := range pkts {
				pkt.Timestamp = rtpTimestamp
				if err := stream.WritePacketRTP(media, pkt); err != nil {
					log.Printf("write RTP failed: %v", err)
					break
				}
			}

			rtpTimestamp += ticksPerFrame
		}
	}
}

func renderFrameJPEG(now time.Time, store *presence.Store, width, height int) ([]byte, bool) {
	view, samples := store.Snapshot(now.UTC())
	img := render.PresenceChart(width, height, now.UTC(), view, samples)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		log.Printf("jpeg encode failed: %v", err)
		return nil, false
	}
	return buf.Bytes(), true
}

func randomUint32() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint32(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint32(b[:])
}

func serveHTTPAPI(ctx context.Context, addr string, store *presence.Store, debug bool) {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v1/samples", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var sample presence.Sample
		if err := json.NewDecoder(r.Body).Decode(&sample); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if err := store.Add(sample); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if debug {
			log.Printf("[debug] sample accepted ts=%s wifi=%d bt=%d", sample.Timestamp.Format(time.RFC3339), sample.WiFiCount, sample.BluetoothCount)
		}
		w.WriteHeader(http.StatusAccepted)
	})

	mux.HandleFunc("/api/v1/view", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if debug {
				log.Printf("[debug] view get")
			}
			_ = json.NewEncoder(w).Encode(store.View())
		case http.MethodPut:
			var view presence.View
			if err := json.NewDecoder(r.Body).Decode(&view); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			if err := store.SetView(view); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if debug {
				log.Printf("[debug] view updated window=%s source=%s mode=%s", view.Window, view.Source, view.DisplayMode)
			}
			_ = json.NewEncoder(w).Encode(view)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	httpSrv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("HTTP API failed: %v", err)
	}
}

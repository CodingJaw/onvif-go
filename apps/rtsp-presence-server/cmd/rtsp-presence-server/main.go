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
	mu     sync.RWMutex
}

func (h *rtspHandler) OnDescribe(_ *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return &base.Response{StatusCode: base.StatusOK}, h.stream, nil
}

func (h *rtspHandler) OnSetup(_ *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return &base.Response{StatusCode: base.StatusOK}, h.stream, nil
}

func (h *rtspHandler) OnPlay(_ *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
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
	)
	flag.Parse()

	store := presence.NewStore()

	mjpegFmt := &format.MJPEG{}
	desc := &description.Session{Medias: []*description.Media{{
		Type:    description.MediaTypeVideo,
		Control: *path,
		Formats: []format.Format{mjpegFmt},
	}}}

	h := &rtspHandler{}
	rtspServer := &gortsplib.Server{
		Handler:        h,
		RTSPAddress:    *rtspAddr,
		UDPRTPAddress:  ":8000",
		UDPRTCPAddress: ":8001",
	}
	if err := rtspServer.Start(); err != nil {
		log.Fatalf("failed to start RTSP server: %v", err)
	}
	defer rtspServer.Close()

	h.stream = &gortsplib.ServerStream{Server: rtspServer, Desc: desc}
	if err := h.stream.Initialize(); err != nil {
		log.Fatalf("failed to initialize RTSP stream: %v", err)
	}
	defer h.stream.Close()

	rtpEnc, err := mjpegFmt.CreateEncoder()
	if err != nil {
		log.Fatalf("failed to create MJPEG encoder: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go serveHTTPAPI(ctx, *httpAddr, store)
	go streamLoop(ctx, h.stream, desc.Medias[0], rtpEnc, store, *fps, *width, *height)

	log.Printf("RTSP stream ready: rtsp://127.0.0.1%s/%s", *rtspAddr, *path)
	log.Printf("HTTP control API: http://127.0.0.1%s/api/v1", *httpAddr)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	cancel()
	time.Sleep(300 * time.Millisecond)
}

func streamLoop(
	ctx context.Context,
	stream *gortsplib.ServerStream,
	media *description.Media,
	rtpEnc *rtpmjpeg.Encoder,
	store *presence.Store,
	fps, width, height int,
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
			view, samples := store.Snapshot(now.UTC())
			img := render.PresenceChart(width, height, now.UTC(), view, samples)

			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
				log.Printf("jpeg encode failed: %v", err)
				continue
			}

			pkts, err := rtpEnc.Encode(buf.Bytes())
			if err != nil {
				log.Printf("rtp encode failed: %v", err)
				continue
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

func randomUint32() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint32(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint32(b[:])
}

func serveHTTPAPI(ctx context.Context, addr string, store *presence.Store) {
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
		w.WriteHeader(http.StatusAccepted)
	})

	mux.HandleFunc("/api/v1/view", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
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

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image/jpeg"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtpmjpeg"
	"github.com/pion/rtp"

	"github.com/0x524a/onvif-go/apps/rtsp-presence-server/internal/presence"
	"github.com/0x524a/onvif-go/apps/rtsp-presence-server/internal/render"
)

type rtspHandler struct {
	server *gortsplib.Server
	stream *gortsplib.ServerStream
	mu     sync.RWMutex
}

func (h *rtspHandler) OnDescribe(_ *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.stream == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, h.stream, nil
}

func (h *rtspHandler) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if ctx.Session.State() == gortsplib.ServerSessionStatePreRecord {
		return &base.Response{StatusCode: base.StatusOK}, nil, nil
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.stream == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, h.stream, nil
}

func (h *rtspHandler) OnPlay(_ *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (h *rtspHandler) OnAnnounce(ctx *gortsplib.ServerHandlerOnAnnounceCtx) (*base.Response, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stream != nil {
		h.stream.Close()
	}
	h.stream = &gortsplib.ServerStream{Server: h.server, Desc: ctx.Description}
	if err := h.stream.Initialize(); err != nil {
		return &base.Response{StatusCode: base.StatusBadRequest}, err
	}
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (h *rtspHandler) OnRecord(ctx *gortsplib.ServerHandlerOnRecordCtx) (*base.Response, error) {
	ctx.Session.OnPacketRTPAny(func(medi *description.Media, _ format.Format, pkt *rtp.Packet) {
		h.mu.RLock()
		stream := h.stream
		h.mu.RUnlock()
		if stream == nil {
			return
		}
		if err := stream.WritePacketRTP(medi, pkt); err != nil {
			log.Printf("route RTP failed: %v", err)
		}
	})

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
		codec    = flag.String("codec", "h264", "stream codec: h264 or mjpeg")
	)
	flag.Parse()

	store := presence.NewStore()

	h := &rtspHandler{}
	rtspServer := &gortsplib.Server{
		Handler:        h,
		RTSPAddress:    *rtspAddr,
		UDPRTPAddress:  ":8000",
		UDPRTCPAddress: ":8001",
	}
	h.server = rtspServer
	if err := rtspServer.Start(); err != nil {
		log.Fatalf("failed to start RTSP server: %v", err)
	}
	defer rtspServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go serveHTTPAPI(ctx, *httpAddr, store)

	switch *codec {
	case "h264":
		if err := runH264Pipeline(ctx, h, *rtspAddr, *path, store, *fps, *width, *height); err != nil {
			log.Fatalf("failed to start h264 pipeline: %v", err)
		}
		log.Printf("RTSP stream ready (H264): rtsp://127.0.0.1%s/%s", *rtspAddr, *path)
	case "mjpeg":
		if err := runMJPEGPipeline(ctx, h, *path, store, *fps, *width, *height); err != nil {
			log.Fatalf("failed to start mjpeg pipeline: %v", err)
		}
		log.Printf("RTSP stream ready (MJPEG): rtsp://127.0.0.1%s/%s", *rtspAddr, *path)
	default:
		log.Fatalf("unsupported codec %q (use h264 or mjpeg)", *codec)
	}

	log.Printf("HTTP control API: http://127.0.0.1%s/api/v1", *httpAddr)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	cancel()
	time.Sleep(300 * time.Millisecond)
}

func runMJPEGPipeline(
	ctx context.Context,
	h *rtspHandler,
	path string,
	store *presence.Store,
	fps, width, height int,
) error {
	mjpegFmt := &format.MJPEG{}
	desc := &description.Session{Medias: []*description.Media{{
		Type:    description.MediaTypeVideo,
		Control: path,
		Formats: []format.Format{mjpegFmt},
	}}}

	h.mu.Lock()
	h.stream = &gortsplib.ServerStream{Server: h.server, Desc: desc}
	if err := h.stream.Initialize(); err != nil {
		h.mu.Unlock()
		return err
	}
	stream := h.stream
	h.mu.Unlock()

	rtpEnc, err := mjpegFmt.CreateEncoder()
	if err != nil {
		return err
	}

	go streamLoopMJPEG(ctx, stream, desc.Medias[0], rtpEnc, store, fps, width, height)

	return nil
}

func runH264Pipeline(
	ctx context.Context,
	h *rtspHandler,
	rtspAddr, path string,
	store *presence.Store,
	fps, width, height int,
) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg not found in PATH; install ffmpeg or run with -codec mjpeg")
	}

	uri := fmt.Sprintf("rtsp://127.0.0.1%s/%s", rtspAddr, path)
	gop := fps * 2
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-loglevel", "error",
		"-re",
		"-f", "mjpeg",
		"-r", fmt.Sprintf("%d", fps),
		"-i", "pipe:0",
		"-an",
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-tune", "zerolatency",
		"-pix_fmt", "yuv420p",
		"-g", fmt.Sprintf("%d", gop),
		"-keyint_min", fmt.Sprintf("%d", gop),
		"-f", "rtsp",
		"-rtsp_transport", "tcp",
		uri,
	)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		_ = stdin.Close()
		_ = cmd.Wait()
	}()
	go streamLoopToMJPEGWriter(ctx, stdin, store, fps, width, height)

	return nil
}

func streamLoopMJPEG(
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
			jpegData, ok := renderFrameJPEG(now, store, width, height)
			if !ok {
				continue
			}

			pkts, err := rtpEnc.Encode(jpegData)
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

func streamLoopToMJPEGWriter(
	ctx context.Context,
	w io.Writer,
	store *presence.Store,
	fps, width, height int,
) {
	if fps <= 0 {
		fps = 5
	}
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
			if _, err := w.Write(jpegData); err != nil {
				log.Printf("ffmpeg pipe write failed: %v", err)
				return
			}
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

var _ = (*rtph264.Encoder)(nil)

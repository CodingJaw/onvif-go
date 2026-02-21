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
	"net"
	"net/http"
	"os"
	"os/exec"
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
	"github.com/pion/rtp"

	"github.com/0x524a/onvif-go/apps/rtsp-presence-server/internal/presence"
	"github.com/0x524a/onvif-go/apps/rtsp-presence-server/internal/render"
)

type rtspHandler struct {
	server    *gortsplib.Server
	stream    *gortsplib.ServerStream
	publisher *gortsplib.ServerSession
	path      string
	debug     bool
	mu        sync.RWMutex
}

func samePath(a, b string) bool {
	return normalizePath(a) == normalizePath(b)
}

func samePathOrTrack(a, b string) bool {
	na := normalizePath(a)
	nb := normalizePath(b)
	return na == nb || strings.HasPrefix(na, nb+"/")
}

func (h *rtspHandler) debugf(format string, args ...interface{}) {
	if h.debug {
		log.Printf("[debug] "+format, args...)
	}
}

func (h *rtspHandler) hasStream() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.stream != nil
}

func (h *rtspHandler) OnRequest(_ *gortsplib.ServerConn, req *base.Request) {
	if req.URL == nil {
		return
	}

	// Some VLC/SAT>IP flows switch to /stream=<id> after an initial request
	// to the configured path. Keep all requests on the configured path so
	// gortsplib session path consistency checks continue to pass.
	if strings.HasPrefix(req.URL.Path, "/stream=") {
		req.URL.Path = "/" + h.path
		h.debugf("rewrote SAT>IP style path to stream path: %s", req.URL.String())
	}

	if req.Method != base.Setup {
		return
	}

	// Some VLC builds (notably without live555) can issue SETUP on the base
	// stream URL without trackID and without trailing slash ("/presence").
	// gortsplib rejects that form before OnSetup. Rewrite to "/presence/"
	// so it is accepted and mapped to track 0 for single-track streams.
	if samePath(req.URL.Path, h.path) &&
		req.URL.RawQuery == "" &&
		!strings.HasSuffix(req.URL.Path, "/") {
		req.URL.Path += "/"
		h.debugf("rewrote SETUP URL to include trailing slash: %s", req.URL.String())
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

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stream != nil && h.publisher != nil && ctx.Session == h.publisher {
		h.stream.Close()
		h.stream = nil
		h.publisher = nil
		h.debugf("publisher disconnected; stream reset")
	}
}

func (h *rtspHandler) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	h.debugf("DESCRIBE path=%s", ctx.Path)
	if !samePath(ctx.Path, h.path) {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.stream == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, h.stream, nil
}

func (h *rtspHandler) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	h.debugf("SETUP path=%s state=%s", ctx.Path, ctx.Session.State())
	if !samePathOrTrack(ctx.Path, h.path) {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
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

func (h *rtspHandler) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	h.debugf("PLAY path=%s", ctx.Path)
	if !samePathOrTrack(ctx.Path, h.path) {
		return &base.Response{StatusCode: base.StatusNotFound}, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (h *rtspHandler) OnAnnounce(ctx *gortsplib.ServerHandlerOnAnnounceCtx) (*base.Response, error) {
	h.debugf("ANNOUNCE path=%s medias=%d", ctx.Path, len(ctx.Description.Medias))
	if !samePath(ctx.Path, h.path) {
		return &base.Response{StatusCode: base.StatusNotFound}, nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.stream != nil {
		h.stream.Close()
	}

	h.stream = &gortsplib.ServerStream{Server: h.server, Desc: ctx.Description}
	if err := h.stream.Initialize(); err != nil {
		return &base.Response{StatusCode: base.StatusBadRequest}, err
	}
	h.publisher = ctx.Session
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (h *rtspHandler) OnRecord(ctx *gortsplib.ServerHandlerOnRecordCtx) (*base.Response, error) {
	h.debugf("RECORD path=%s", ctx.Path)
	if !samePath(ctx.Path, h.path) {
		return &base.Response{StatusCode: base.StatusNotFound}, nil
	}
	ctx.Session.OnPacketRTPAny(func(medi *description.Media, _ format.Format, pkt *rtp.Packet) {
		h.mu.RLock()
		stream := h.stream
		h.mu.RUnlock()
		if stream == nil {
			return
		}
		if err := stream.WritePacketRTP(medi, pkt); err != nil {
			log.Printf("route RTP failed: %v", err)
		} else {
			h.debugf("RTP forwarded pt=%d ts=%d seq=%d", pkt.PayloadType, pkt.Timestamp, pkt.SequenceNumber)
		}
	})

	return &base.Response{StatusCode: base.StatusOK}, nil
}

func main() {
	var (
		rtspAddr = flag.String("rtsp-addr", ":8554", "RTSP bind address")
		httpAddr = flag.String("http-addr", ":18080", "HTTP API bind address")
		path     = flag.String("path", "presence", "RTSP path")
		fps      = flag.Int("fps", 15, "stream FPS")
		width    = flag.Int("width", 1280, "video width")
		height   = flag.Int("height", 720, "video height")
		host     = flag.String("host", "127.0.0.1", "host/IP for displayed stream/API URLs")
		pubHost  = flag.String("publish-host", "127.0.0.1", "host/IP used by internal H264 publisher to connect RTSP server")
		codec    = flag.String("codec", "h264", "stream codec: h264 or mjpeg")
		debug    = flag.Bool("debug", false, "enable verbose debug logging")
	)
	flag.Parse()
	*path = normalizePath(*path)
	if *host == "0.0.0.0" {
		log.Printf("warning: -host 0.0.0.0 is not reachable by clients; use a concrete IP/DNS name for advertised URL")
	}

	store := presence.NewStore()
	h := &rtspHandler{debug: *debug, path: *path}

	rtspServer := &gortsplib.Server{
		Handler:           h,
		RTSPAddress:       *rtspAddr,
		UDPRTPAddress:     ":8000",
		UDPRTCPAddress:    ":8001",
		MulticastIPRange:  "224.1.0.0/16",
		MulticastRTPPort:  8002,
		MulticastRTCPPort: 8003,
	}
	h.server = rtspServer
	if err := rtspServer.Start(); err != nil {
		log.Fatalf("failed to start RTSP server: %v", err)
	}
	defer rtspServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go serveHTTPAPI(ctx, *httpAddr, store, *debug)

	switch *codec {
	case "mjpeg":
		if err := runMJPEGPipeline(ctx, h, *path, store, *fps, *width, *height, *debug); err != nil {
			log.Fatalf("failed to start mjpeg pipeline: %v", err)
		}
		log.Printf("RTSP stream ready (MJPEG): rtsp://%s%s/%s", *host, *rtspAddr, *path)
	case "h264":
		if err := runH264Pipeline(ctx, h, *pubHost, *rtspAddr, *path, store, *fps, *width, *height, *debug); err != nil {
			log.Fatalf("failed to start h264 pipeline: %v", err)
		}
		log.Printf("RTSP stream ready (H264): rtsp://%s%s/%s", *host, *rtspAddr, *path)
	default:
		log.Fatalf("unsupported codec %q (use h264 or mjpeg)", *codec)
	}

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

func runMJPEGPipeline(
	ctx context.Context,
	h *rtspHandler,
	path string,
	store *presence.Store,
	fps, width, height int,
	debug bool,
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

	go streamLoopMJPEG(ctx, stream, desc.Medias[0], rtpEnc, store, fps, width, height, debug)
	return nil
}

func runH264Pipeline(
	ctx context.Context,
	h *rtspHandler,
	host, rtspAddr, path string,
	store *presence.Store,
	fps, width, height int,
	debug bool,
) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg not found in PATH; install ffmpeg or run with -codec mjpeg")
	}
	publishHost := normalizePublishHost(host)
	publishAddr := net.JoinHostPort(publishHost, extractPortOrDefault(rtspAddr, "8554"))
	if err := waitForTCP(ctx, publishAddr, 5*time.Second); err != nil {
		return fmt.Errorf("rtsp listener not ready at %s: %w", publishAddr, err)
	}

	uri := fmt.Sprintf("rtsp://%s:%s/%s", publishHost, extractPortOrDefault(rtspAddr, "8554"), path)
	gop := max(10, fps*2)
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-loglevel", "error",
		"-fflags", "+genpts",
		"-re",
		"-f", "mjpeg",
		"-r", fmt.Sprintf("%d", fps),
		"-i", "pipe:0",
		"-an",
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-tune", "zerolatency",
		"-profile:v", "baseline",
		"-pix_fmt", "yuv420p",
		"-colorspace", "bt709",
		"-color_primaries", "bt709",
		"-color_trc", "bt709",
		"-b:v", "2500k",
		"-maxrate", "2500k",
		"-bufsize", "5000k",
		"-g", fmt.Sprintf("%d", gop),
		"-keyint_min", fmt.Sprintf("%d", gop),
		"-fps_mode", "cfr",
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
	if debug {
		log.Printf("[debug] ffmpeg started pid=%d uri=%s", cmd.Process.Pid, uri)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- cmd.Wait()
	}()
	go func() {
		<-ctx.Done()
		_ = stdin.Close()
	}()
	go streamLoopToMJPEGWriter(ctx, stdin, store, fps, width, height, debug)

	readyDeadline := time.NewTimer(15 * time.Second)
	defer readyDeadline.Stop()
	pollTicker := time.NewTicker(100 * time.Millisecond)
	defer pollTicker.Stop()

	for {
		if h.hasStream() {
			if debug {
				log.Printf("[debug] h264 publisher announced stream successfully")
			}
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			if err != nil {
				return fmt.Errorf("h264 publisher exited before announce: %w", err)
			}
			return fmt.Errorf("h264 publisher exited before announce")
		case <-readyDeadline.C:
			return fmt.Errorf("h264 publisher did not announce stream within timeout")
		case <-pollTicker.C:
		}
	}
}

func normalizePublishHost(host string) string {
	h := strings.TrimSpace(host)
	if h == "" || h == "0.0.0.0" || h == "::" {
		return "127.0.0.1"
	}
	return h
}

func extractPortOrDefault(addr, fallback string) string {
	if strings.HasPrefix(addr, ":") {
		return strings.TrimPrefix(addr, ":")
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return fallback
	}
	return port
}

func waitForTCP(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timeout waiting for %s", addr)
		case <-ticker.C:
		}
	}
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
		fps = 15
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

func streamLoopToMJPEGWriter(
	ctx context.Context,
	w io.Writer,
	store *presence.Store,
	fps, width, height int,
	debug bool,
) {
	if fps <= 0 {
		fps = 15
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
			if debug {
				log.Printf("[debug] wrote frame to ffmpeg bytes=%d", len(jpegData))
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

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

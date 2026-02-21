# RTSP Presence Server (first building block)

This subfolder contains a standalone RTSP app focused on the hardest part first: producing a VLC-playable stream.

## What it does now

- Hosts a **gortsplib-native RTSP stream** with selectable codec mode:
  - `h264` (default, better compatibility)
  - `mjpeg` (simple fallback/debug)
- Renders a simple chart frame from incoming presence samples.
- Exposes a small HTTP API so Python (or Go ONVIF server) can push data and change view mode.

> Why this mode?
> - It uses `gortsplib` directly as the RTSP producer/server path.
> - It works with VLC / ffplay and many other RTSP clients while keeping implementation simple and observable.

## Run

```bash
go run ./apps/rtsp-presence-server/cmd/rtsp-presence-server -codec h264
```

Set a different IP/host (instead of `127.0.0.1`) with:

```bash
go run ./apps/rtsp-presence-server/cmd/rtsp-presence-server -host 192.168.1.50
```

If you pass `-host 0.0.0.0`, the server still binds to all interfaces, but the internal H264 publisher auto-uses `127.0.0.1` to publish into RTSP locally.

If the server advertises a non-local host but runs on the same machine as the internal H264 publisher,
set publisher host explicitly:

```bash
go run ./apps/rtsp-presence-server/cmd/rtsp-presence-server -codec h264 -host 192.168.1.50 -publish-host 127.0.0.1
```



Codec modes:
- `-codec h264` (default): gortsplib server + ffmpeg x264 publisher (widest RTSP client compatibility)
- `-codec mjpeg`: pure gortsplib MJPEG producer path (simple fallback/debug)

H264 defaults are tuned for smoother playback and better metadata signaling:
- default `-fps` is `15` (instead of 5)
- encoder emits BT.709 color metadata (`color_primaries` / `color_transfer`)
- includes a default silent AAC audio track (`-audio-source silent`) for better VLC compatibility

Audio options (H264 mode):
- `-audio-source silent` (default): inject synthetic silent AAC
- `-audio-source pulse [-audio-device <source>]`: capture from PulseAudio input
- `-audio-source alsa [-audio-device <device>]`: capture from ALSA input
- `-audio-source none`: disable audio track entirely

Examples:

```bash
# default silent AAC
go run ./apps/rtsp-presence-server/cmd/rtsp-presence-server -codec h264

# PulseAudio mic/source (use pactl list short sources to discover names)
go run ./apps/rtsp-presence-server/cmd/rtsp-presence-server -codec h264 -audio-source pulse -audio-device default

# ALSA capture
go run ./apps/rtsp-presence-server/cmd/rtsp-presence-server -codec h264 -audio-source alsa -audio-device hw:0,0
```

> H264 mode requires `ffmpeg` in `PATH`.

Debug mode:
- add `-debug` to print RTSP events (`DESCRIBE/SETUP/PLAY/ANNOUNCE/RECORD`, session/connection open/close), frame pipeline activity, and HTTP API updates.

Transport compatibility:
- RTSP over TCP
- RTP/RTCP over UDP
- UDP multicast

Path handling:
- `-path` accepts `presence` or `/presence` (normalized internally), to avoid client/path mismatches.

RTSP URI (default):

```text
rtsp://<host>:8554/presence
```

Open in VLC: `Media -> Open Network Stream -> rtsp://<host>:8554/presence`

### VLC package caveat (important)

Some distro VLC builds (including Ubuntu 24.04 package `3.0.20-3build6`) are compiled with:

- `--disable-live555`
- `--enable-realrtsp`

In that build, VLC tries SAT>IP / RealRTSP handlers for `rtsp://...` and can fail with logs like:

- `satip stream error: Failed to setup RTSP session`
- `access_realrtsp stream warning: only real/helix rtsp servers supported for now`

That failure is a **VLC client build limitation**, not an RTSP server transport issue.

Check your VLC build quickly:

```bash
cvlc -vvv --play-and-exit rtsp://<host>:8554/presence
```

If logs show `--disable-live555`, use one of:

- `ffplay -rtsp_transport tcp rtsp://<host>:8554/presence`
- a VLC build/package with live555-enabled RTSP support

### ffprobe "unknown" fields on this stream

`ffprobe -show_streams` will show several fields as `N/A` or `unknown` for this feed (for example `duration`, `bit_rate`, `color_transfer`, `color_primaries`).
That is expected for a **live RTSP stream** coming from an ongoing H264 encoder pipeline; those fields are often unavailable or not signaled in SDP.
It does **not** indicate a broken stream by itself.

Also, for RTSP/RTP H264, `is_avc=false` and `nal_length_size=0` are expected in ffprobe output because RTP packetization carries Annex-B style NAL units rather than MP4/AVCC length-prefixed NALs.

If your client previously showed `461 Unsupported Transport`, that was caused by missing UDP transport listeners. The server now supports both UDP and TCP interleaved RTSP transport.

RTSP stream expectations (what this server provides):
- SDP via `DESCRIBE`
- `SETUP` + `PLAY` for readers
- for H264 mode, internal publisher uses `ANNOUNCE` + `RECORD`
- RTP timestamps and sequence numbers per packet
- transport support: TCP interleaved, UDP unicast, UDP multicast

For lower startup latency with ffplay:

```bash
ffplay -fflags nobuffer -flags low_delay -rtsp_transport tcp rtsp://<host>:8554/presence
```

The stream is updated in real-time when you POST samples or change `/api/v1/view`.
The x-axis is time-based (sample timestamps against the active window), so changing `live`/`10m`/`1h` changes horizontal scaling, and the chart continues to scroll left as time passes even without new samples.

## ffplay warning notes (MJPEG mode)

If ffplay prints warnings like:

```text
[swscaler] deprecated pixel format used, make sure you did set range correctly
```

this is a known ffmpeg/swscale message when decoding **MJPEG** (`yuvj420p` full-range) and converting for display. The stream is still valid.

Use this command to keep latency low and hide warning spam:

```bash
ffplay -loglevel error -fflags nobuffer -flags low_delay -rtsp_transport tcp rtsp://<host>:8554/presence
```

If you need totally warning-free playback in ffmpeg logs, a future optional H264/H265 encoder path can be added.

## HTTP API

Base URL: `http://<host>:18080/api/v1`

### Add sample

```bash
curl -X POST http://<host>:18080/api/v1/samples \
  -H 'content-type: application/json' \
  -d '{"wifi_count":17,"bluetooth_count":9}'
```

### Get active view

```bash
curl http://<host>:18080/api/v1/view
```

### Change active view

```bash
curl -X PUT http://<host>:18080/api/v1/view \
  -H 'content-type: application/json' \
  -d '{"window":"10m","source":"wifi","display_mode":"bar"}'
```

Allowed values:
- `window`: `live`, `10m`, `1h`, `6h`, `12h`, `24h`
- `source`: `both`, `wifi`, `bluetooth`
- `display_mode`: `line`, `bar`, `histogram`, `text`

## How this connects back to onvif-go

- Keep this app as the RTSP/video engine.
- In ONVIF server config, point profile stream URI to this RTSP path.
- ONVIF side handles standards/control surface; this app handles frame rendering + transport.

A good near-term integration path is:
1. ONVIF app owns PTZ menu state.
2. ONVIF app updates this RTSP app via HTTP/gRPC view-control API.
3. Python producer sends samples here directly (or to ONVIF first, then forwarded).

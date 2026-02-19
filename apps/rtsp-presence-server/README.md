# RTSP Presence Server (first building block)

This subfolder contains a standalone RTSP app focused on the hardest part first: producing a VLC-playable stream.

## What it does now

- Hosts an **RTSP MJPEG** stream (easy to validate with VLC).
- Renders a simple chart frame from incoming presence samples.
- Exposes a small HTTP API so Python (or Go ONVIF server) can push data and change view mode.

> Why MJPEG first?
> - It is fully valid RTSP video and easy to inspect while the control/data contracts are being built.
> - Next step can switch encoder to H264/H265 once we lock chart/layout/control behavior.

## Run

```bash
go run ./apps/rtsp-presence-server/cmd/rtsp-presence-server
```

RTSP URI (default):

```text
rtsp://127.0.0.1:8554/presence
```

Open in VLC: `Media -> Open Network Stream -> rtsp://127.0.0.1:8554/presence`

If your client previously showed `461 Unsupported Transport`, that was caused by missing UDP transport listeners. The server now supports both UDP and TCP interleaved RTSP transport.

For lower startup latency with ffplay:

```bash
ffplay -fflags nobuffer -flags low_delay -rtsp_transport tcp rtsp://127.0.0.1:8554/presence
```

The stream is updated in real-time when you POST samples or change `/api/v1/view`.
The x-axis is time-based (sample timestamps against the active window), so changing `live`/`10m`/`1h` changes horizontal scaling, and the chart continues to scroll left as time passes even without new samples.

## HTTP API

Base URL: `http://127.0.0.1:18080/api/v1`

### Add sample

```bash
curl -X POST http://127.0.0.1:18080/api/v1/samples \
  -H 'content-type: application/json' \
  -d '{"wifi_count":17,"bluetooth_count":9}'
```

### Get active view

```bash
curl http://127.0.0.1:18080/api/v1/view
```

### Change active view

```bash
curl -X PUT http://127.0.0.1:18080/api/v1/view \
  -H 'content-type: application/json' \
  -d '{"window":"10m","source":"wifi"}'
```

Allowed values:
- `window`: `live`, `10m`, `1h`, `6h`, `12h`, `24h`
- `source`: `both`, `wifi`, `bluetooth`

## How this connects back to onvif-go

- Keep this app as the RTSP/video engine.
- In ONVIF server config, point profile stream URI to this RTSP path.
- ONVIF side handles standards/control surface; this app handles frame rendering + transport.

A good near-term integration path is:
1. ONVIF app owns PTZ menu state.
2. ONVIF app updates this RTSP app via HTTP/gRPC view-control API.
3. Python producer sends samples here directly (or to ONVIF first, then forwarded).

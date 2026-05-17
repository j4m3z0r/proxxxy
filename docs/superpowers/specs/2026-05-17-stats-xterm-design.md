# proxxxy: Compression Stats API & xterm End-to-End

**Date:** 2026-05-17
**Status:** Approved

---

## Overview

Two milestones:

1. **Compression stats** — expose live compression metrics via an HTTP endpoint on the server, consumed by the `proxxxy-ctl` CLI and the `watch` command.
2. **xterm end-to-end** — verify and fix the relay stack so that `xterm` renders correctly and responds to keyboard input. Test with screenshots at each stage.

---

## Milestone 1: Compression Stats

### Counters

A new `Stats` struct in `internal/compress` holds per-connection counters using `sync/atomic` (`atomic.Int64`). Fields:

| Field | Meaning |
|---|---|
| `BytesIn` | Raw X11 bytes seen from the app |
| `BytesOut` | Bytes actually written to the wire (after compression) |
| `DictHits` | `ActionRef` events — client already has entry |
| `DictDefines` | `ActionDefine` events — first send to client |
| `DictPassthroughs` | `ActionPassthrough` events — sequence too large for dict |
| `TemplateHits` | Template apply events |
| `TemplateDefines` | Template define events |

`Encoder` embeds `*Stats` and increments counters in `Encode()` and `encodeOne()`. `Encoder.GetStats()` returns a snapshot (value copy of each atomic).

**Note:** While Phase 3 encoding is currently bypassed, `BytesIn` and `BytesOut` are tracked in the `sendFn` closure inside `relayAppToClient` — `BytesIn` is `len(x11_request)` before the call, `BytesOut` is `len(x11_request)` after (equal while bypassed, will diverge when Phase 3 is active). Dict/template counters remain 0 until Phase 3 is wired in.

### Encoder Registry

`Server` gains a `encoders sync.Map` (key: `uint32` connID, value: `*compress.Encoder`). `relayAppToClient` registers the encoder on connect and removes it on disconnect. The stats handler iterates the map to collect per-connection snapshots.

### HTTP Stats Server

The server starts a `net/http` listener on port `statsPort` (default `tcpPort+1`, i.e. 7101 when main port is 7100). One handler is registered:

```
GET /stats
GET /stats?aggregate=1
```

**Full response** (`/stats`):

```json
{
  "client_connected": true,
  "active_connections": 2,
  "aggregate": {
    "bytes_in": 1048576,
    "bytes_out": 204800,
    "ratio": 0.805,
    "dict_hits": 1523,
    "dict_defines": 234,
    "dict_passthroughs": 45,
    "template_hits": 89,
    "template_defines": 12
  },
  "connections": {
    "1": {
      "bytes_in": 900000,
      "bytes_out": 175000,
      "ratio": 0.806,
      "dict_hits": 1400,
      "dict_defines": 200,
      "dict_passthroughs": 40,
      "template_hits": 80,
      "template_defines": 10
    },
    "2": { "..." : "..." }
  }
}
```

**Aggregate-only response** (`/stats?aggregate=1`): same shape but `connections` key omitted.

`ratio` = `bytes_out / bytes_in` (0.0–1.0; lower means better compression). When `bytes_in` is 0, ratio is reported as `1.0`.

The stats port is added as a `-stats-port` flag to `cmd/server/main.go` (default `0` = `tcp-port + 1`).

### ctl Tool Rewrite

`cmd/ctl/main.go` is rewritten to:

- Use `net/http` (`http.Get`) instead of `net.Dial`
- Flag `-server` changes meaning to the base URL: default `http://localhost:7101`
- Flag `-aggregate` (bool): appends `?aggregate=1` to the request URL
- Pretty-print the JSON response with `json.MarshalIndent`

**Watch usage:**
```bash
watch -n1 proxxxy-ctl                   # full stats
watch -n1 'proxxxy-ctl -aggregate'      # aggregate only
```

---

## Milestone 2: xterm End-to-End

### Goal

`xterm` running on the fake display (`:2`) renders its window, accepts keyboard input, and echoes typed characters correctly. The relay stack (server on port 7100, client forwarding to display `:1`) must be transparent to xterm's X11 usage.

### Test Methodology

At each stage, build all binaries, start the stack, launch xterm, and take a screenshot via `scrot` or `import` to verify visually. Stages:

1. **Render**: `xterm` window appears with a visible terminal. No content required — just the window frame and background.
2. **Shell prompt**: A shell prompt is visible inside xterm.
3. **Keystroke echo**: Typed characters appear in xterm (verified by sending keystrokes via `xdotool` and screenshotting).

### Likely Issues and Fixes

The relay is already bidirectional and handles all X11 opcodes opaquely — `drainRequests` reads any 4-byte header and uses the length field regardless of opcode. However, xterm exercises parts of the stack the testclient does not:

1. **Connection setup response timing**: xterm blocks on the setup response before sending any requests. The existing relay handles this (client opens real display, relays response back), but needs verification.

2. **Large protocol messages**: xterm sends `InternAtom`, `GetProperty`, `ChangeProperty` etc. early in setup. These are request-response pairs relayed transparently — no changes expected.

3. **X11 state model crashes**: `ProcessRequest` in `x11/state.go` must not panic on unknown opcodes. It should silently ignore what it doesn't recognise. This needs auditing and defensive hardening if needed.

4. **Event flow for interactivity**: xterm connects to the proxy's Unix socket (fake display `:2`). The proxy forwards xterm's X11 requests to the real display `:1` via the client. Events — key presses, expose, focus, etc. — are generated by the real display and flow back through the reverse path: real display → `relayXToServer` in client → `MsgX11Data` over TCP → server `readFromClient` → xterm's socket. Both directions of this pipe must work correctly for interactive input. `xdotool` (running on `:1`) can inject synthetic key events to test this without a physical keyboard.

5. **Event relay correctness**: Events from the real display come back via `relayXToServer` (client reads from real display, sends `MsgX11Data` to server) → server `readFromClient` writes to app socket. This path must correctly route events to the right xterm connection.

### Build and Run Commands

```bash
# Build
go build -o /tmp/proxxxy-server ./cmd/server/
go build -o /tmp/proxxxy-client ./cmd/client/
go build -o /tmp/proxxxy-ctl    ./cmd/ctl/

# Clean stale state
rm -f /tmp/.X11-unix/X2 /tmp/.X2-lock

# Start stack (display :1 = real Xorg; :2 = fake/proxy)
DISPLAY=:1 /tmp/proxxxy-server -display 2 -port 7100 &
DISPLAY=:1 /tmp/proxxxy-client -server localhost:7100 &

# Launch xterm on the fake display
DISPLAY=:2 xterm &

# Screenshot
DISPLAY=:1 scrot /tmp/screenshot.png

# Stats
watch -n1 /tmp/proxxxy-ctl
```

---

## Testing

- Stats: unit test `compress.Stats` counter increments; integration test that `/stats` returns valid JSON with correct aggregate values.
- xterm relay: visual confirmation via screenshots; `go test ./...` must continue to pass.

---

## Out of Scope

- Font re-opening during session resume (existing known gap).
- Phase 3 encoder activation (blocked on client decoder — separate milestone).
- `cmd/ctl` stats port follows server's main port +1 by convention; no auto-discovery.

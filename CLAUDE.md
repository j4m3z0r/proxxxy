# proxxxy

Resumable, delta-compressed X11 proxy in Go. Lets remote X11 sessions survive network interruptions and reconnect without losing window state.

**Module:** `james.id.au/proxxxy` (Go 1.24)
**Key deps:** `github.com/BurntSushi/xgb`, `github.com/cespare/xxhash/v2`

## Architecture

Three-phase design (Phases 1 and 2 complete; Phase 3 built but client-side decoder not yet written):

```
X11 app → Unix socket (fake display :N) → proxxxy-server → TCP → proxxxy-client → Unix socket (real display)
```

**Phase 1** — Opaque relay. Server presents a fake X display, client forwards to the real one.

**Phase 2** — State model. Server tracks X11 objects (windows, GCs, pixmaps) so it can synthesise the display state for a reconnecting client.

**Phase 3** — Delta compression. LRU dictionary (xxHash fingerprinting), parametric templates, dirty-region coalescing. Encoder is implemented but **bypassed** — see Known Gap below.

## Directory Map

```
cmd/server/       proxxxy-server binary (flags: -display N, -port P)
cmd/client/       proxxxy-client binary (flags: -server host:port)
cmd/testclient/   minimal X11 app: 300×300 window, 200×200 rect toggling white/black every 1s
cmd/ctl/          thin stats CLI stub (dials localhost:7101; server stats endpoint not implemented)
internal/wire/    wire framing: 5-byte header [type:1][length:4 LE], 64 MB max
internal/server/  Server struct, session parsing, Phase 2 synthesis
internal/client/  Client struct, local display relay
internal/x11/     opcode constants, request header parser, X11 state model, remap tables
internal/compress/ Dict, TemplateRegistry, RegionTracker, Encoder
docs/superpowers/specs/  design doc
docs/superpowers/plans/  original 17-task implementation plan
```

## Wire Protocol

Message format: `[type:1][length:4 LE][payload…]`

| Constant | Value | Purpose |
|---|---|---|
| `MsgHello` | 0x01 | version handshake (payload: uint32 LE version) |
| `MsgSessionResume` | 0x02 | synthesis begins |
| `MsgSessionLive` | 0x03 | synthesis done, live relay starts |
| `MsgX11Data` | 0x04 | raw X11 bytes: `[conn_id:4 LE][x11 bytes…]` |
| `MsgDictDefine` | 0x10 | new dict entry: `[id:8 LE][bytes…]` |
| `MsgDictRef` | 0x11 | dict hit: `[id:8 LE]` |
| `MsgDictExpire` | 0x12 | eviction notice: `[id:8 LE]` |
| `MsgTemplateDefine` | 0x13 | new template |
| `MsgTemplateApply` | 0x14 | apply template with params |
| `MsgRegionAck` | 0x20 | reserved |

`ProtocolVersion = 1`

## Running It

```bash
# Real display lives at :1 (xorg-dummy). Fake display at :2.
/tmp/proxxxy-server -display 2 -port 7100 &
DISPLAY=:1 /tmp/proxxxy-client -server localhost:7100 &
DISPLAY=:2 /tmp/proxxxy-testclient &
```

Build binaries:
```bash
go build -o /tmp/proxxxy-server   ./cmd/server/
go build -o /tmp/proxxxy-client   ./cmd/client/
go build -o /tmp/proxxxy-testclient ./cmd/testclient/
```

Clean up stale sockets before starting: `rm -f /tmp/.X11-unix/X2 /tmp/.X2-lock`

## Key Types

**`internal/server.Server`** — core server. Fields: `clientConn net.Conn` (current proxxxy-client, nil if none), `clientW sync.Mutex` (serialises client writes), `appConns map[uint32]net.Conn`, `appState map[uint32]*x11.AppConn`.

**`internal/x11.AppConn`** — per-connection X11 state. Snapshot methods (`Windows()`, `GCs()`, `Pixmaps()`) return value copies with deep-copied slices (race-safe). `ProcessRequest([]byte)` updates state from a raw X11 request.

**`internal/x11.Window`**, **`GC`**, **`Pixmap`** — value types with `CreateReq() []byte` (returns the raw X11 request that recreates the object) and `DrawCmds [][]byte` (replay log for pixmaps).

**`internal/compress.Encoder`** — wraps Dict + TemplateRegistry + RegionTracker. `Encode(windowID, cmd, order) []wire.Msg`, `DrainExpiredDicts() []wire.Msg`, `OnClientDisconnect()`.

**`internal/compress.Dict`** — xxHash LRU, server-authoritative. `Classify(seq) (Action, id, data)` returns `ActionDefine`, `ActionRef`, or `ActionPassthrough`. `DrainExpired() []uint64`.

## Known Gap — Phase 3 Encoder Bypassed

The encoder (`compress.Encoder`) is fully implemented and tested but **not wired into the relay path**. It was connected in commit `4313def`, but that broke the demo because the client only handles `MsgX11Data` — it has no decoder for `MsgDictDefine`, `MsgDictRef`, `MsgTemplateDefine`, or `MsgTemplateApply`.

**Current state:** `drainRequests` in `internal/server/session.go` ignores the encoder parameter (`_`) and calls `sendFn(full)` directly, forwarding raw X11 bytes as `MsgX11Data`. This is the correct behaviour for Phase 1+2 relay.

**To fully activate Phase 3**, both of these are needed:
1. Implement a client-side decoder in `internal/client/client.go` that handles `MsgDictDefine`, `MsgDictRef`, `MsgDictExpire`, `MsgTemplateDefine`, `MsgTemplateApply`.
2. Fix the protocol: `MsgDictDefine`/`MsgDictRef`/`MsgTemplateApply` have no `conn_id` field, so the client cannot route decoded data to the right X connection. Need to add `conn_id` to those message payloads (or use a per-connection channel).

## Other Known Gaps

- **`cmd/ctl`** stats CLI dials `localhost:7101` and expects JSON, but the server has no stats endpoint. Stub only.
- **Font state** not tracked in `AppConn` — fonts opened before reconnect won't be re-opened during synthesis.
- **No tests** for `internal/client`.

## Test Suite

```bash
go test ./...
# ok  james.id.au/proxxxy/internal/compress
# ok  james.id.au/proxxxy/internal/server   (includes e2e_test.go: uses display :97, port 17197)
# ok  james.id.au/proxxxy/internal/wire
# ok  james.id.au/proxxxy/internal/x11
```

## Design Docs

- Spec: `docs/superpowers/specs/2026-05-16-proxxxy-design.md`
- Plan: `docs/superpowers/plans/2026-05-17-proxxxy.md` (17 tasks, all implemented)

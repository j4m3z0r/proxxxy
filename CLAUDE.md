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
cmd/xlog/          debug tool: sniffs X11 traffic on a display and logs opcodes/extensions
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
go build -o /tmp/proxxxy-server   ./cmd/server/
go build -o /tmp/proxxxy-client   ./cmd/client/
go build -o /tmp/proxxxy-testclient ./cmd/testclient/

rm -f /tmp/.X11-unix/X2 /tmp/.X2-lock
/tmp/proxxxy-server -display 2 -port 7100 > /tmp/server.log 2>&1 &
DISPLAY=:1 /tmp/proxxxy-client -server localhost:7100 > /tmp/client.log 2>&1 &
DISPLAY=:2 /tmp/proxxxy-testclient &
```

### GIMP testing

GIMP is the primary real-world test app. It exercises the full synthesis path including MIT-SHM, RENDER, SYNC, depth-32 ARGB windows, large requests (BigRequests), and complex window hierarchies.

```bash
# Start server on display :2, connect client (display :1 = real), launch GIMP on :2
go build -o /tmp/proxxxy-server ./cmd/server/ && \
go build -o /tmp/proxxxy-client ./cmd/client/

rm -f /tmp/.X11-unix/X2 /tmp/.X2-lock
/tmp/proxxxy-server -display 2 -port 7100 > /tmp/server.log 2>&1 &
sleep 0.3
DISPLAY=:1 /tmp/proxxxy-client -server localhost:7100 > /tmp/client.log 2>&1 &
sleep 0.3
DISPLAY=:2 gimp &

# Reconnect test: kill client, wait, reconnect
pkill -f proxxxy-client; sleep 1
DISPLAY=:1 /tmp/proxxxy-client -server localhost:7100 >> /tmp/client.log 2>&1 &
```

**What to verify after reconnect:**
- GIMP windows appear and render correctly (not black/grey)
- Clicking menus and tools works (input events flow)
- Multiple reconnects don't degrade state

**If it's broken**, check `/tmp/client.log` for X errors:
- `code=3 major=1 badID=0xNNN` → BadWindow from CreateWindow: orphan windows with dead parent (handleDestroyWindow cascading issue)
- `code=8 major=72/130 badID=0xNNN` → BadMatch from PutImage/ShmPutImage: depth mismatch from failed synthesis
- `code=8 major=2 badID=0xNNN` → BadValue from ChangeWindowAttributes: invalid event mask bits

## Key Types

**`internal/server.Server`** — core server. Fields: `clientConn net.Conn` (current proxxxy-client, nil if none), `clientW sync.Mutex` (serialises client writes), `appConns map[uint32]net.Conn`, `appState map[uint32]*x11.AppConn`, `synthActive bool` (true during runSynthesis; blocks live relay).

**`internal/x11.AppConn`** — per-connection X11 state. Snapshot methods (`Windows()`, `GCs()`, `Pixmaps()`) return value copies with deep-copied slices (race-safe). `ProcessRequest([]byte)` updates state from a raw X11 request.

Key methods:
- `ResetPendingConfigures()` — called at synthesis start to zero configure counts
- `DrainPendingConfigures() map[uint32]uint32` — atomically returns and resets per-window configure counts
- `ShmSegs()`, `ShmPixmaps()`, `SyncCounters()` — snapshots for synthesis replay

**`internal/x11.Window`** — value type with:
- `CreateReq() []byte` — raw X11 request that recreates the window
- `EventMask uint32` — current XSelectInput mask (tracked via CreateWindow + ChangeWindowAttributes)
- `PendingConfigures uint32` — ConfigureWindow requests since last reset; used to inject the right number of ConfigureNotify events during synthesis to drain GTK3's `configure_request_count`
- `Children []uint32` — child window IDs (maintained by handleCreateWindow/handleDestroyWindow)

**`internal/x11.GC`**, **`internal/x11.Pixmap`** — value types with `CreateReq() []byte` and `DrawCmds [][]byte` (replay log for pixmaps).

**`internal/compress.Encoder`** — wraps Dict + TemplateRegistry + RegionTracker. `Encode(windowID, cmd, order) []wire.Msg`, `DrainExpiredDicts() []wire.Msg`, `OnClientDisconnect()`.

**`internal/compress.Dict`** — xxHash LRU, server-authoritative. `Classify(seq) (Action, id, data)` returns `ActionDefine`, `ActionRef`, or `ActionPassthrough`. `DrainExpired() []uint64`.

## Synthesis Deep Dive

Synthesis runs in `server/synthesis.go` → `runSynthesis()` → `synthesiseAppConn()` per connection. Order matters:

1. `ResetPendingConfigures()` on all connections (so DrainPendingConfigures later is accurate)
2. Send `MsgX11Setup` (connection setup bytes + ridBase/ridMask/appSeqNum)
3. SHM segments (`ShmAttach` replay)
4. SYNC counters (`SyncCreateCounter` replay)
5. Windows: parent-first walk via `findRoots` + `synthesiseWindowCreate`
   - Each window: `CreateWindow` → `ChangeWindowAttributes` (SelectInput, to replay event masks)
6. Pixmaps (create only)
7. SHM pixmaps (recreated as regular pixmaps with matching depth/size)
8. Fonts (`OpenFont` replay)
9. Core cursors (`CreateCursor`/`CreateGlyphCursor`, skipping those whose source pixmap/font is gone)
10. GCs (`CreateGC` + `ChangeGC` replay, with sanitize passes for dead drawables/pixmaps/fonts)
11. RENDER Pictures + GlyphSets
11c. RENDER cursors (`RenderCreateCursor`): after Pictures since they reference a source Picture. If the source picture's backing drawable is gone (e.g. Firefox creates a pixmap-backed picture for a cursor then immediately frees both), falls back to a glyph cursor from the "cursor" font — the ID exists so live traffic doesn't get BadCursor, but the cursor shape won't match.
12. Pixmap draw commands (filtered: skip commands referencing freed GCs)
13. Map windows
14. Send `MsgSessionLive` (with finalSeqNum per connID)
15. Clear `synthActive`
16. For each connection: `DrainPendingConfigures` → inject N `ConfigureNotify` events → inject `Expose`

The client (`client.go`) runs `synthRelay` in three phases:
- **Phase 1**: drain synthesis xconn until `done` is closed (SESSION_LIVE received)
- **Phase 2**: send GetInputFocus barrier, drain until its reply; capture `nSynth`
- **Phase 3**: forward all messages with seq rewriting: `seqOffset = uint16(finalSeqNum) - nSynth`

### Why ConfigureNotify injection

GTK3/GDK tracks `configure_request_count` per toplevel window. It increments this when sending `ConfigureWindow` and decrements on `ConfigureNotify`. If the client disconnects mid-session, GIMP keeps sending `ConfigureWindow` (which proxxxy discards) but never gets `ConfigureNotify` back — the count stays > 0, which permanently blocks `gdk_window_process_updates_with_mode` from drawing. We inject `max(1, pendingCfgs[wid])` ConfigureNotify events per window to drain it.

**Critical invariant:** ConfigureNotify injection is restricted to **toplevel windows only** (windows whose parent is NOT another app-owned window). Injecting ConfigureNotify for internal child windows (e.g. 1×1 GDK helper windows) corrupts GTK3's widget allocation, producing "Negative content width" layout crashes. The check in `synthesisConfigureNotify` is: `if _, parentIsApp := windows[w.Parent]; parentIsApp { continue }`.

### Why SelectInput replay

GTK3 calls `XSelectInput` (= `ChangeWindowAttributes` with `CWEventMask`) AFTER `CreateWindow` to add `ButtonPressMask`, `KeyPressMask`, `PointerMotionMask`, etc. Synthesis only replayed `CreateWindow` (which typically has only `ExposureMask`|`StructureNotifyMask`), so synthesis xconn windows received no input events from the real X server. We now track `EventMask` on `Window` and replay it as `ChangeWindowAttributes` right after each `CreateWindow` during synthesis.

### Why destroyWindowTree cascades

`DestroyWindow` in X11 destroys the entire subtree. Previously `handleDestroyWindow` only removed the specified window. Orphan children remained in `appState` with a stale parent. On the next synthesis, `findRoots` treated orphans as roots and tried to create them with their (now non-existent) parent → `BadWindow`. `destroyWindowTree` now recursively removes all descendants.

## Remote X server (XQuartz) compatibility

proxxxy is normally used as a *remote* proxy (app + server on one host, proxxxy-client + real X server on another, e.g. a Mac running XQuartz). Two XQuartz facts broke synthesis until fixed:

### Dynamic extension major opcodes

Extension major opcodes are **server-assigned, not fixed**. X.org and XQuartz disagree: RENDER 139→**141**, MIT-SHM 130→**132**, SYNC 134→**135**, XInput 131→**133**. The hardcoded `Opcode*` constants in `internal/x11/opcodes.go` are only correct for X.org/Xvfb. `AppConn` now **learns** each extension's real opcode by pairing `QueryExtension` requests (recorded in `ProcessRequest` → `pendingQueryExt[seq]=name`) with their replies (parsed in `server.go readFromClient` → `LearnQueryExtensionReply`, populating `extByOpcode`). `ProcessRequest`'s default case dispatches SYNC/MIT-SHM/XInput by learned opcode. (RENDER also has an independent `dynamicRenderOpcode` learned from the first `RenderCreatePicture`.)

**Why this matters most for SYNC:** GTK's frame clock creates an XSync counter and issues `SyncSetCounter` (SYNC minor 3) on it every frame. With the hardcoded SYNC opcode (134) wrong on XQuartz (135), `handleSYNC` never fired → the counter was never tracked → synthesis never recreated it → after reconnect `SyncSetCounter` referenced a dead counter → `BadCounter` (code=138) → **GIMP/Firefox crashed** (window vanished). With the learned opcode, the counter is tracked and replayed (`SyncCounters()` → synthesis step 4), so it survives reconnect.

### Extension reply-faking during the disconnect window

While no client is connected (or during synthesis), app requests are dropped and `OpcodeNeedsReply` injects a fake reply for **core** reply-expecting requests so XLib's `_XReply` doesn't block. Extension requests were not covered, so a synchronous extension request in that window (notably XKB `XkbGetMap`, opcode 135/136) hung the whole app → black window after reconnect. `AppConn.ExtNeedsReply(opcode, minor)` now uses the learned opcode + per-extension reply-minor tables (`extReplyMinors`, covering XKB/RENDER/SYNC/XFIXES/DAMAGE/RANDR/MIT-SHM/XInput); `drainRequests` fakes replies for those too.

### Capture caveat for testing on XQuartz

`xwd -id` returns an all-black image for a composited window once it has **settled** (no backing store), but captures correctly in the brief window right after the app redraws (post-Expose). Test tooling must capture rapidly right after a reconnect and keep the brightest frame — a settled read falsely looks black. (`xwd -root` and `screencapture` of X coordinates are both useless under rootless XQuartz.)

## injectColormap (client-side synthesis)

During synthesis (`inSynthesis=true`), depth-32 `CreateWindow` requests that lack `CWColormap` get a scratch colormap injected by `injectColormap`. The original colormap ID refers to a dead connection; without an explicit colormap the real X server returns `BadMatch`. Scratch colormaps are allocated from the top of the synthesis xconn's resource range (counting down from `ridBase | ridMask`) and cached by visual ID in `synthXconnState`.

## Scratch pixmap creation (server-side synthesis)

When synthesising a GC whose original drawable was freed before disconnect, `sanitizeGCDrawable` normally substitutes any surviving drawable of matching depth. If none exists (e.g. the only depth-8 resources were temporary pixmaps), the GC gets synthesised against a wrong-depth drawable — causing `BadMatch` when the app later issues `PutImage` against a correct-depth drawable using that GC. To prevent this, `synthesiseAppConn` creates a 1×1 scratch pixmap of the required depth before the GC synthesis loop, so `sanitizeGCDrawable` always finds a depth-match.

### Scratch ID range reservation

Both server scratch pixmaps and client scratch colormaps allocate IDs from the top of the synthesis xconn's resource range, counting downward. To avoid collision:
- **Client scratch colormaps** (`injectColormap`): top quarter, from `ridBase | ridMask` downward (IDs in the range `ridMask - ridMask/4 + 1 .. ridMask`).
- **Server scratch pixmaps** (`synthesiseAppConn`): upper-middle, starting from `ridBase | (ridMask - ridMask/4)` downward.

The bottom three-quarters of the ridMask range belong to normal application resources.

## applyIDRemap (client-side synthesis)

When `oldBase != newBase` (the new X connection got a different resource-id-base), all synthesised resource IDs must be remapped. `applyIDRemap` handles: CreateWindow, ChangeWindowAttributes (CWCursor), ConfigureWindow, CreatePixmap, CreateGC (including GC value-list bits 10/11/14/19), ChangeGC, CopyGC, OpenFont/CloseFont, CreateCursor/CreateGlyphCursor, FreeCursor, CopyArea, CopyPlane, all RENDER opcodes, and MIT-SHM opcodes (ShmPutImage shmseg at offset 32, ShmCreatePixmap shmseg at offset 20).

## applyEventReverseRemap (client-side synthesis)

`applyIDRemap` remaps outgoing synthesis requests (app's old IDs → synthesis xconn's new IDs). But **incoming events** from the real display carry new-base IDs — the app, which is still using old-base IDs, won't recognise them and drops the events.

`applyEventReverseRemap` is the inverse: called in `synthRelay` Phase 3 on every event (bytes[0] ≥ 2) when `oldBase != newBase`, it rewrites new-base IDs back to old-base. It handles all core X11 event types with window/drawable fields (KeyPress, ButtonPress, MotionNotify, Enter/LeaveNotify, Expose, CreateNotify, DestroyNotify, MapNotify, ReparentNotify, ConfigureNotify, ConfigureRequest, PropertyNotify, ColormapNotify, ClientMessage, etc.) and XI2 GenericEvents (type 35, evtype 2–10: event window at +24, child at +28).

This was discovered when Chromium reconnects got a different ridBase — all keyboard events were silently dropped because Chromium's window IDs in the events didn't match the windows it had registered.

## Known Gap — Phase 3 Encoder Bypassed

The encoder (`compress.Encoder`) is fully implemented and tested but **not wired into the relay path**. It was connected in commit `4313def`, but that broke the demo because the client only handles `MsgX11Data` — it has no decoder for `MsgDictDefine`, `MsgDictRef`, `MsgTemplateDefine`, or `MsgTemplateApply`.

**Current state:** `drainRequests` in `internal/server/session.go` ignores the encoder parameter (`_`) and calls `sendFn(full)` directly, forwarding raw X11 bytes as `MsgX11Data`. This is the correct behaviour for Phase 1+2 relay.

**To fully activate Phase 3**, both of these are needed:
1. Implement a client-side decoder in `internal/client/client.go` that handles `MsgDictDefine`, `MsgDictRef`, `MsgDictExpire`, `MsgTemplateDefine`, `MsgTemplateApply`.
2. Fix the protocol: `MsgDictDefine`/`MsgDictRef`/`MsgTemplateApply` have no `conn_id` field, so the client cannot route decoded data to the right X connection. Need to add `conn_id` to those message payloads (or use a per-connection channel).

## Other Known Gaps

- **`cmd/ctl`** stats CLI dials `localhost:7101` and expects JSON, but the server has no stats endpoint. Stub only.
- **Font state** is tracked. `AppConn` records each `OpenFont`/`CloseFont` with the raw request bytes. Synthesis replays `OpenFont` before `CreateGC` (step 2.5), and `sanitizeGCFont` strips `GCFont` from any `CreateGC` whose font was closed before reconnect. `applyIDRemap` in the client also remaps the `GCFont` value-list slot for `CreateGC`/`ChangeGC` when ridBase changes.
- **Cursor state** is tracked. `AppConn` records each `CreateCursor`/`CreateGlyphCursor`/`FreeCursor`. Synthesis replays them in step 2.6 (after pixmaps and fonts which they reference), skipping any whose source pixmap/font is gone. `sanitizeCreateWindow` strips `CWCursor` (bit 14) from `CreateWindow` since cursors are synthesized after windows. `applyIDRemap` remaps cursor IDs in `ChangeWindowAttributes` and `CreateWindow` value lists when ridBase changes.
- **No tests** for `internal/client`.
- **DrawCmds may reference freed GCs** — synthesis replays pixmap draw history which may include commands sent with GCs that were later freed. The synthesis now skips such commands (GC liveness filter in step 5), but pixmap content may be slightly incomplete until the Expose-triggered redraw.
- **GDK freeze/thaw assertion** — after reconnect, GIMP may log `gdk_window_thaw_toplevel_updates: assertion 'window->update_and_descendants_freeze_count > 0' failed`. This is a non-fatal `g_return_if_fail` warning (the function returns without taking action). It occurs when the WM's `_NET_WM_SYNC_REQUEST` protocol and our ConfigureNotify injection interact during the reconnect window. Drawings still happen; the warning is cosmetic.
- **GPU extensions (DRI2/DRI3/Present) suppressed** — these extensions use file-descriptor passing (SCM_RIGHTS ancillary data) to share dma-buf GPU buffers, which proxxxy cannot forward over its TCP wire protocol. The client intercepts `QueryExtension` replies for these extensions and rewrites `present` to `false`, forcing apps to fall back to software GL (Mesa llvmpipe for Firefox, SwiftShader for Chrome). GLX is intentionally NOT suppressed — it works with software Mesa without FD passing. WebGL still works via software renderers; only hardware GPU acceleration is unavailable. Under Xvfb (used in tests) this suppression is a no-op since Xvfb doesn't advertise these extensions.
- **XKB BadAccess (major=135)** — apps that use XKEYBOARD may generate `code=10 major=135` errors during synthesis. This is expected: XKB requires a per-connection `XkbUseExtension` handshake before any other XKB request; the synthesis xconn doesn't perform this handshake. The errors are non-fatal — apps fall back to core X11 keyboard handling. The E2E test harness filters `major=135` from the error check. Attempting to call `XkbUseExtension` on the synthesis xconn causes Xvfb to send `XkbNewKeyboardNotify` events that are then forwarded to the app, breaking keyboard routing — so don't try to fix this by enabling XKB on the synthesis xconn.

## Test Suite

```bash
go test ./...
# ok  james.id.au/proxxxy/internal/compress
# ok  james.id.au/proxxxy/internal/server   (includes e2e_test.go: uses display :97, port 17197)
# ok  james.id.au/proxxxy/internal/wire
# ok  james.id.au/proxxxy/internal/x11
```

### E2E reconnect harness (`tests/run_stage.sh`)

Shell harness that drives full reconnect lifecycles on Xvfb. Each stage runs a real app through N reconnect cycles and verifies no X errors appear.

```bash
tests/run_stage.sh --stage 9 --reconnects 3
# Results in tests/results/YYYYMMDD-HHMMSS-stage9/
```

| Stage | App | What it tests | POST_RECONNECT_SETTLE |
|---|---|---|---|
| 1 | testclient | Baseline: minimal window, pixmap redraws | 2s |
| 2–6 | GIMP | Full synthesis path: SHM, RENDER, depth-32 ARGB, complex hierarchies | varies |
| 7 | Firefox | Browser rendering, URL navigation (xdotool type in address bar) | 20s |
| 8 | Chromium | Browser rendering + ridBase-change event remap, URL navigation | 10s |
| 9 | LibreOffice | Format menu open/close via xdotool | 3s |

The `POST_RECONNECT_SETTLE` delay is how long the harness waits after synthesis before attempting post-reconnect input (menus, URL bar). Firefox/Chromium GPU compositors restart their renderer processes on reconnect and need extra time.

## Design Docs

- Spec: `docs/superpowers/specs/2026-05-16-proxxxy-design.md`
- Plan: `docs/superpowers/plans/2026-05-17-proxxxy.md` (17 tasks, all implemented)

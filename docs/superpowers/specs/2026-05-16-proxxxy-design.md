# proxxxy Design Spec

**Date:** 2026-05-16
**Status:** Approved

## Overview

proxxxy is an X11 proxy that allows remote X applications to display on a local machine,
with support for session resume (client disconnect and reconnect without losing running apps)
and high-efficiency delta compression of the X11 command stream.

It differs from SSH X11 forwarding in three ways:
1. The server maintains X app connections even when no client is connected
2. The protocol applies semantic delta compression (not pixel-level) for low bandwidth
3. Communication is over plain TCP (no SSH dependency, no firewall complications)

## Architecture

```
Remote machine                          Local machine
──────────────────────────────────      ──────────────────────────────
  X app (DISPLAY=:10)
      │ X11 protocol (Unix socket)
      ▼
  ┌─────────────────────┐               ┌─────────────────────┐
  │   proxxxy-server    │               │   proxxxy-client    │
  │                     │  TCP :7100    │                     │
  │  X11 listener :10   │◄─────────────►│  TCP connector      │
  │  State model        │               │  X11 forwarder      │
  │  Session store      │               │                     │
  └─────────────────────┘               └─────────────────────┘
                                                 │ X11 protocol
                                                 ▼
                                         Local X display :1
```

### Binaries

- **proxxxy-server** — runs on the remote machine. Listens on a Unix socket presenting a
  fake X display (`:10` by default). Accepts X app connections, maintains a full X11 state
  model, and serves proxxxy-client connections over TCP port 7100.

- **proxxxy-client** — runs on the local machine. Connects to the server over TCP, connects
  to the local real X display, and relays X11 traffic bidirectionally.

- **proxxxy-ctl** — CLI for inspecting server state, listing sessions, viewing per-connection
  statistics (bytes forwarded, dict hit rate, template hit rate, commands coalesced).

### Development Phases

- **Phase 1:** Opaque byte relay. Server forwards raw bytes to the first connected client
  with no protocol parsing. Proves the plumbing works.

- **Phase 2:** X11 protocol parsing, full state model, and session resume. Client can
  disconnect and reconnect; the server synthesizes the current state for the new client.

- **Phase 3:** Delta compression. Command sequence dictionary, parametric templates, and
  region-aware dirty tracking reduce bandwidth by >80% for typical animated GUI workloads.

### Test Application

Rather than using `xclock` (which uses the RENDER and SHAPE extensions), Phase 1 and 2
testing uses a purpose-built minimal Go test client (`cmd/testclient`) built on
`BurntSushi/xgb`. It creates a window containing a filled rectangle that changes colour
once per second, using only core X11 protocol — no extensions. This gives a controlled,
predictable workload. Extension support (starting with RENDER) is added in Phase 2 once
the core proxy is solid, enabling `xclock` as a secondary test target.

## Wire Protocol (proxxxy ↔ proxxxy-client)

### Framing

All messages use the same envelope:

```
[type: 1 byte][length: 4 bytes little-endian][payload: length bytes]
```

### Version

Both sides exchange a `HELLO` message at connection start. The version field is a single
`uint32`. If the client and server versions do not match exactly, the receiving side sends
an error response and closes the connection. Version negotiation is deferred to a future
revision.

### Message Types

| Type | Value | Direction | Payload encoding | Purpose |
|------|-------|-----------|-----------------|---------|
| `HELLO` | 0x01 | both | protobuf | Version + capability negotiation |
| `SESSION_RESUME` | 0x02 | server→client | protobuf | Start of synthesized state replay |
| `SESSION_LIVE` | 0x03 | server→client | protobuf | Replay complete, switching to live |
| `X11_DATA` | 0x04 | both | protobuf header + raw bytes | X11 bytes for one app connection |
| `DICT_DEFINE` | 0x10 | server→client | protobuf + raw | Assign ID to command sequence |
| `DICT_REF` | 0x11 | server→client | protobuf | Replay dictionary entry N |
| `DICT_EXPIRE` | 0x12 | server→client | protobuf | Client must free dictionary entry N |
| `TEMPLATE_DEFINE` | 0x13 | server→client | protobuf | Define parametric template |
| `TEMPLATE_APPLY` | 0x14 | server→client | protobuf | Apply template with delta parameters |
| `REGION_ACK` | 0x20 | client→server | protobuf | Client has rendered a screen region |

Control messages (`HELLO`, session signals, dict/template messages, `REGION_ACK`) use
protobuf-encoded payloads defined in `proto/proxxxy.proto`. `X11_DATA` carries raw X11
bytes directly — no encoding overhead on the hot path.

Each `X11_DATA` message carries a protobuf-encoded header containing `conn_id` (identifying
which of the server's active X app connections the bytes belong to) followed immediately by
the raw X11 bytes. The client uses `conn_id` to route bytes to the correct local X
connection. The raw X11 bytes are not protobuf-encoded — they follow the header directly
to avoid encoding overhead.

### Handshake Sequence

```
client → HELLO(version, dict_capacity_bytes)
server → HELLO(version, dict_capacity_bytes)   ← errors and closes if version mismatch
server → SESSION_RESUME
server → X11_DATA × N                           ← synthesized state (empty on first connect)
server → SESSION_LIVE
         [live bidirectional relay begins]
```

## X11 State Model (Phase 2)

### Per-Connection State

The server maintains one `AppConn` per X application connected to the fake display:

```
AppConn
├── byteOrder          (big or little endian, negotiated at X11 handshake)
├── seqNum             (current X11 sequence number from the app)
├── resourceIDBase     (app's allocated resource ID range)
├── resourceIDMask
├── seqMap             map[client_seqnum → app_seqnum]   (in-flight reply mapping)
├── atomMap            map[original_atom_id → new_atom_id]
├── windows            map[id] → Window
├── gcs                map[id] → GC
├── pixmaps            map[id] → Pixmap
├── fonts              map[id] → Font
└── colormaps          map[id] → Colormap
```

### Resource Types

**Window:**
```
id, parent, x, y, width, height, borderWidth
class, visual, depth
attributes (background pixel/pixmap, event mask, cursor, ...)
mapped bool
children []id   (ordered)
properties map[atom_id][]byte
```

**GC (Graphics Context):**
```
id, drawable
values: foreground, background, lineWidth, lineStyle, capStyle,
        joinStyle, fillStyle, fillRule, tile, stipple, font,
        subwindowMode, graphicsExposures, clipMask, ...
```

**Pixmap:**
```
id, drawable, width, height, depth
drawCmds []RawCommand   (ordered log of all drawing operations applied)
```

**Font:** `id, name`

**Colormap:** `id, visual, entries map[pixel]RGB`

### Parsing Strategy

X11 request format: `[opcode:1][pad:1][length:2][data...]` (length in 4-byte units).
The parser reads opcode + length on every request, extracts resource IDs from fixed
offsets (opcode-specific), and updates state. Raw bytes are stored verbatim — no
re-serialisation needed for relay. The parser does not fully decode every field; it
decodes only what is needed to maintain state.

Extensions are handled by intercepting `QueryExtension` replies to learn opcode mappings,
then applying extension-specific parsers. Phase 2 implements the RENDER extension parser
(enabling `xclock` as a test target). Other extensions are forwarded opaquely.

### Sequence Number and Atom Remapping

Resource IDs (window, GC, pixmap, font) are client-assigned in X11 and transfer without
remapping. Atom IDs are server-assigned, so they differ between the original and reconnect
X server sessions.

**Sequence number mapping:** `seqMap` tracks `client_seqnum → app_seqnum` for all
in-flight reply-generating requests. Synthesis requests that generate replies (e.g.,
`InternAtom` calls during reconnect) map to `→ discard` — the reply is consumed silently
to update `atomMap`. Live app requests map to their original sequence number; replies are
rewritten before forwarding to the app.

**Atom remapping:** During synthesis the server sends `InternAtom` for each known atom
name, populates `atomMap` with `original_id → new_id`, then rewrites all atom ID fields
in subsequent requests and replies.

## Session Resume Synthesis (Phase 2)

When a proxxxy-client connects, the server sends `SESSION_RESUME` then replays synthesized
X11 commands for each `AppConn`, in this order:

1. **Connection setup bytes** — original display parameters and auth reply
2. **Atoms** — `InternAtom` for each known atom name; populate `atomMap`
3. **Fonts** — `OpenFont` for each open font (Phase 2: require font parity; log warning if
   the font is unavailable on the client's X server and fall back to `fixed`)
4. **Colormaps** — `CreateColormap` + `AllocColor` for each entry
5. **Pixmaps** — `CreatePixmap` then replay `drawCmds[]` in order
6. **GCs** — `CreateGC` with all stored attribute values
7. **Windows** — walk tree parent-first:
   - `CreateWindow` with stored geometry and attributes
   - `ChangeProperty` for each stored property
   - `ConfigureWindow` if needed
   - `MapWindow` if mapped
8. **Input focus** — `SetInputFocus` to the stored focused window

After all `AppConn`s are synthesized, the server sends `SESSION_LIVE` and switches to live
relay. While no client is connected, the server continues accepting commands from X apps
and updating state so the model is always current at reconnect time.

## Delta Compression (Phase 3)

Three mechanisms applied to the server→client data path:

### Level 2 — Command Sequence Dictionary

Both sides maintain a shared dictionary keyed by ever-incrementing `uint64` IDs. The
server is authoritative: it decides when to add and when to evict entries. The client
never independently evicts.

- **Add:** server sends `DICT_DEFINE(id, bytes)`. Client stores `id → bytes`.
- **Reference:** server sends `DICT_REF(id)`. Client replays stored bytes.
- **Evict:** server sends `DICT_EXPIRE(id)`. Client frees the entry.

The server computes an xxHash fingerprint of each outgoing command sequence:
- Not in `globalDict`: send raw bytes, add to `globalDict`
- In `globalDict` but not in `clientDict`: send `DICT_DEFINE`, add to `clientDict`
- In both: send `DICT_REF`

On reconnect, `clientDict` resets to empty. The client's dictionary is rebuilt organically
as live traffic flows — no bulk transfer, no reconnect latency spike.

Server LRU-evicts the oldest `globalDict` entry (sending `DICT_EXPIRE` first) whenever a
new entry would exceed the configured cache capacity (default 64MB). IDs are never reused.

### Level 3 — Parametric Templates

When two command sequences share identical opcode structure but differ only in numeric
parameters (coordinates, colours, arc angles), the server extracts a *template*: the byte
sequence with numeric parameter slots replaced by offsets. Subsequent instances send
`TEMPLATE_APPLY(template_id, params[])` instead of the full bytes.

Template candidates are detected by a secondary pass comparing sequences that hash
differently but share opcode structure. Effective for periodic animations where the draw
sequence is structurally identical every frame.

### Level 5 — Region-Aware Dirty Tracking and Command Coalescing

The server maintains a dirty region set per window. The client sends `REGION_ACK` once it
has rendered a region. The server holds back commands for already-clean regions and
coalesces superseded commands before transmission (e.g., `ClearArea` followed by
`PolyFillRectangle` covering the same area collapses to just the `PolyFillRectangle`).

### Expected Gains

For a typical animated GUI (periodic redraw of mostly-static content):
- Level 2 alone: ~70-80% bandwidth reduction after warm-up
- Level 3 adds: further reduction for animation-heavy workloads
- Level 5 adds: reduction for apps that redraw redundantly within a frame

Target: >80% overall reduction for the animated rectangle test workload within one or two
animation cycles of reconnect.

## Testing

| Phase | Test | Pass Criteria |
|-------|------|---------------|
| 1 | `testclient` → proxy server + client → local display (localhost) | Rectangle appears, changes colour, no corruption |
| 2 | Disconnect `proxxxy-client`, wait 5s, reconnect | Rectangle reappears in current colour (not initial) |
| 3 | Measure bytes transmitted over 60s with/without compression | >80% reduction for animated rectangle workload |

`proxxxy-ctl` exposes per-connection stats for Phase 3 validation: bytes forwarded,
`DICT_REF` hit rate, template hit rate, commands coalesced.

## Known Limitations and Deferred Work

- **Font parity:** Phase 2 requires the same fonts to be installed on client and server
  machines. Cross-machine font data transfer (via `QueryFont` + synthetic bitmap font
  creation) is deferred to a later phase.
- **Extension support:** Only core X11 + RENDER in Phase 2. Other extensions (SHAPE, XKB,
  RANDR) are forwarded opaquely and may not survive session resume correctly.
- **Protocol version negotiation:** Phase 1/2 require exact version match. Backward-
  compatible negotiation is deferred.
- **Authentication:** No authentication on the TCP connection in Phase 1/2. Deferred.
- **Multiple simultaneous proxxxy-client connections:** Not supported; the server accepts
  one client at a time.

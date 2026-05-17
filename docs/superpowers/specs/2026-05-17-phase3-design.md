# proxxxy: Phase 3 — Delta Compression (Levels 2 & 3)

**Date:** 2026-05-17
**Status:** Approved

---

## Overview

Activate delta compression for the relay path. This milestone implements Level 2 (LRU dictionary) and Level 3 (parametric templates). Level 5 (region coalescing) is deferred to a later phase — the current Add→Flush-immediately pattern provides no real batching benefit without an ACK-based flush protocol.

The encoder (`compress.Encoder`) is already implemented and tested. This milestone wires it into the relay path on the server side and adds the matching decoder on the client side.

---

## Wire Protocol Changes

`ProtocolVersion` bumps from 1 to 2.

Every compressed message gains a `[connID:4 LE]` prefix so the client can route decoded bytes to the correct X connection. `MsgX11Data` is unchanged — it already carries `[connID:4]`.

| Message | Old payload | New payload |
|---|---|---|
| `MsgDictDefine` | `[id:8][data…]` | `[connID:4][id:8][data…]` |
| `MsgDictRef` | `[id:8]` | `[connID:4][id:8]` |
| `MsgDictExpire` | `[id:8]` | `[connID:4][id:8]` |
| `MsgTemplateDefine` | `[id:8][nslots:2][slots…][base…]` | `[connID:4][id:8][nslots:2][slots…][base…]` |
| `MsgTemplateApply` | `[id:8][nparams:2][params…]` | `[connID:4][id:8][nparams:2][params…]` |

---

## Encoder Side

### `internal/compress/compress.go`

All five `make*` message-builder functions gain a `connID uint32` parameter. The connID is prepended as 4 bytes LE before the existing payload in each case.

`DrainExpiredDicts` also gains a connID prefix on `MsgDictExpire` payloads (the encoder already holds `e.connID`).

### `internal/server/session.go`

`drainRequests` is updated:

- The encoder parameter changes from `_` to `enc` (it is now used).
- `sendFn` changes type from `func([]byte)` to `func([]wire.Msg)`.
- For each request, call `enc.Encode(0, full, ac.Order)` and pass the resulting `[]wire.Msg` to `sendFn`.
- `DrainExpiredDicts()` is called after each `Encode` call; any returned messages are also passed to `sendFn`.

### `internal/server/server.go`

`relayAppToClient` closure is updated:

- `BytesIn` is incremented by `len(full)` before `Encode`.
- `BytesOut` is incremented by the sum of payload lengths across all returned `wire.Msg` values.
- Each `wire.Msg` is written to the client via `wire.Write`.

`sendToClient` is updated to accept and write `[]wire.Msg` instead of `[]byte`.

---

## Decoder — `internal/compress/decoder.go`

A new `Decoder` type, symmetric with `Encoder`, holds per-connection state:

```go
type Decoder struct {
    dict      map[uint64][]byte
    templates map[uint64]*Template
}

func NewDecoder() *Decoder
func (d *Decoder) Decode(msg wire.Msg) (connID uint32, data []byte, err error)
```

Behaviour by message type:

| Message | Action | Returns data? |
|---|---|---|
| `MsgDictDefine` | Store `dict[id] = data` | Yes — the raw command is forwarded to X11 |
| `MsgDictRef` | Look up `dict[id]` | Yes — the stored command |
| `MsgDictExpire` | Delete `dict[id]` | No |
| `MsgTemplateDefine` | Parse slots and base, store template | No |
| `MsgTemplateApply` | Call `tmpl.Apply(params)` | Yes — the reconstructed command |

`Template.Apply(params [][]byte) ([]byte, error)` already exists in the compress package; the decoder calls it directly.

`Decode` returns `(connID, nil, nil)` for define/expire messages. The caller skips writing to X11 when `data == nil`.

`MsgX11Data` is not handled by `Decode`; the client handles it directly as before.

---

## Client Side — `internal/client/client.go`

`Client` gains `decoders map[uint32]*compress.Decoder` (initialised in `NewClient`).

In `relayServerToX`, the message type switch is expanded:

- For `MsgDictDefine`, `MsgDictRef`, `MsgDictExpire`, `MsgTemplateDefine`, `MsgTemplateApply`: look up or create a `Decoder` for the connID extracted from the payload, call `decoder.Decode(msg)`, and if `data != nil`, write to `xConns[connID]`.
- `MsgX11Data` handling is unchanged.

Decoders are created on first use (lazy init). There is no explicit cleanup — decoders are lightweight and the map is bounded by the number of active connections.

---

## Stats

Stat counters (`DictHits`, `DictDefines`, `DictPasses`, `TemplateHits`, `TemplateDefs`) are incremented inside `encodeOne`, which is now called from the live relay path. `BytesIn` and `BytesOut` will now diverge for compressible workloads, making the ratio in `/stats` meaningful.

---

## Testing

### Round-trip test (`internal/compress/`)

A new `TestRoundTrip` encodes a sequence of X11 commands through `NewEncoder` → `Encode`, feeds each resulting `wire.Msg` through `NewDecoder().Decode`, and asserts the output bytes match the original input. Exercises `ActionDefine`, `ActionRef`, template define, and template apply paths.

### `internal/server/e2e_test.go`

The fake client in the e2e test already accepts all compressed message types. Update the ProtocolVersion check from 1 to 2.

### `internal/server/server_test.go`

No structural changes needed. If any test constructs wire messages manually, update them for the new connID prefix.

---

## Wire Helpers — `internal/wire/wire.go`

`ProtocolVersion` constant changes from 1 to 2.

The decoder needs to extract `connID` from the start of compressed message payloads. Add parse helpers mirroring `ParseX11Data`:

```go
func ParseCompressedMsg(payload []byte) (connID uint32, rest []byte, err error)
```

Returns the leading 4-byte connID and the remainder of the payload. Used by `Decoder.Decode` for all five compressed message types.

---

## Out of Scope

- Level 5 region coalescing (deferred — requires ACK-based flush protocol).
- Font state tracking in `AppConn` (pre-existing gap).
- Session resumption with compressed state (Phase 2 synthesis replays raw X11; compression is per-session only).

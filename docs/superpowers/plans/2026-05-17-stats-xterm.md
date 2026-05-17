# Stats API & xterm End-to-End Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an HTTP compression-stats endpoint visible via `watch`, then get xterm rendering and interactive on the proxied display.

**Architecture:** Atomic counters embedded in `compress.Encoder` are polled by a `net/http` handler on the stats port (default: main-port+1). The server holds a `sync.Map` of live encoders, keyed by connID. `cmd/ctl` is rewritten as an HTTP client. xterm is tested iteratively with screenshots after each fix.

**Tech Stack:** Go stdlib (`net/http`, `sync/atomic`, `httptest`), `scrot`/`xdotool` for xterm verification.

---

## File Map

| Action | Path | Purpose |
|---|---|---|
| Create | `internal/compress/stats.go` | `Stats` struct (atomic counters) + `Snapshot` value type |
| Create | `internal/compress/stats_test.go` | Unit tests for `Stats.Snapshot()` |
| Modify | `internal/compress/compress.go` | Add `Stats *Stats` to `Encoder`; increment dict/template counters in `encodeOne` |
| Modify | `internal/server/server.go` | Add `statsPort`, `statsHTTP`, `encoders sync.Map`; update `New()`, `Start()`, `Stop()` |
| Modify | `internal/server/session.go` | Register encoder in `encoders`; wrap `sendFn` to count `BytesIn`/`BytesOut` |
| Create | `internal/server/stats.go` | JSON types + `handleStats` HTTP handler |
| Create | `internal/server/stats_test.go` | Unit tests for `handleStats` via `httptest` |
| Modify | `internal/server/server_test.go` | Update `New()` call sites to pass stats port |
| Modify | `internal/server/e2e_test.go` | Update `New()` call site to pass stats port |
| Modify | `cmd/server/main.go` | Add `-stats-port` flag |
| Rewrite | `cmd/ctl/main.go` | HTTP client + `-aggregate` flag |

---

## Task 1 — `compress.Stats` struct and `Snapshot` type

**Files:**
- Create: `internal/compress/stats.go`
- Create: `internal/compress/stats_test.go`

- [ ] **Step 1: Create `internal/compress/stats.go`**

```go
package compress

import "sync/atomic"

// Stats holds per-connection compression counters. Safe for concurrent use.
type Stats struct {
	BytesIn      atomic.Int64
	BytesOut     atomic.Int64
	DictHits     atomic.Int64
	DictDefines  atomic.Int64
	DictPasses   atomic.Int64
	TemplateHits atomic.Int64
	TemplateDefs atomic.Int64
}

// Snapshot is a point-in-time copy of Stats.
type Snapshot struct {
	BytesIn      int64
	BytesOut     int64
	DictHits     int64
	DictDefines  int64
	DictPasses   int64
	TemplateHits int64
	TemplateDefs int64
}

// Snapshot returns a consistent point-in-time copy of all counters.
func (s *Stats) Snapshot() Snapshot {
	return Snapshot{
		BytesIn:      s.BytesIn.Load(),
		BytesOut:     s.BytesOut.Load(),
		DictHits:     s.DictHits.Load(),
		DictDefines:  s.DictDefines.Load(),
		DictPasses:   s.DictPasses.Load(),
		TemplateHits: s.TemplateHits.Load(),
		TemplateDefs: s.TemplateDefs.Load(),
	}
}
```

- [ ] **Step 2: Write the failing test `internal/compress/stats_test.go`**

```go
package compress

import "testing"

func TestStatsSnapshot(t *testing.T) {
	s := &Stats{}
	s.BytesIn.Add(1000)
	s.BytesOut.Add(800)
	s.DictHits.Add(5)
	s.DictDefines.Add(3)
	s.DictPasses.Add(1)
	s.TemplateHits.Add(7)
	s.TemplateDefs.Add(2)

	snap := s.Snapshot()

	checks := []struct {
		name string
		got  int64
		want int64
	}{
		{"BytesIn", snap.BytesIn, 1000},
		{"BytesOut", snap.BytesOut, 800},
		{"DictHits", snap.DictHits, 5},
		{"DictDefines", snap.DictDefines, 3},
		{"DictPasses", snap.DictPasses, 1},
		{"TemplateHits", snap.TemplateHits, 7},
		{"TemplateDefs", snap.TemplateDefs, 2},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %d want %d", c.name, c.got, c.want)
		}
	}
}
```

- [ ] **Step 3: Run tests to verify they pass**

```bash
go test ./internal/compress/ -run TestStatsSnapshot -v
```

Expected output: `PASS`

- [ ] **Step 4: Commit**

```bash
git add internal/compress/stats.go internal/compress/stats_test.go
git commit -m "feat: add compress.Stats with atomic counters and Snapshot"
```

---

## Task 2 — Wire `Stats` into `compress.Encoder`

**Files:**
- Modify: `internal/compress/compress.go`

- [ ] **Step 1: Add `Stats *Stats` field to `Encoder` and initialise in `NewEncoder`**

In `internal/compress/compress.go`, update the `Encoder` struct and `NewEncoder`:

```go
type Encoder struct {
	connID    uint32
	dict      *Dict
	templates *TemplateRegistry
	regions   *RegionTracker
	prev      map[byte][]byte
	Stats     *Stats
}

func NewEncoder(connID uint32, dictCapacity int) *Encoder {
	return &Encoder{
		connID:    connID,
		dict:      NewDict(dictCapacity),
		templates: NewTemplateRegistry(),
		regions:   NewRegionTracker(),
		prev:      make(map[byte][]byte),
		Stats:     &Stats{},
	}
}
```

- [ ] **Step 2: Increment dict counters in `encodeOne`**

In `encodeOne`, after `action, id, data := e.dict.Classify(cmd)`, update the switch:

```go
switch action {
case ActionDefine:
	e.Stats.DictDefines.Add(1)
	return []wire.Msg{makeDictDefine(id, data)}
case ActionRef:
	e.Stats.DictHits.Add(1)
	return []wire.Msg{makeDictRef(id)}
}
// ActionPassthrough
e.Stats.DictPasses.Add(1)
p := make([]byte, 4+len(cmd))
binary.LittleEndian.PutUint32(p[:4], e.connID)
copy(p[4:], cmd)
return []wire.Msg{{Type: wire.MsgX11Data, Payload: p}}
```

- [ ] **Step 3: Increment template counters in `encodeOne`**

In `encodeOne`, in the template detection block (before the dict classification), update:

```go
if tmpl != nil {
	e.prev[opcode] = cmd
	if isNew {
		e.Stats.TemplateDefs.Add(1)
		return []wire.Msg{
			makeTemplateDefine(tmpl),
			makeTemplateApply(tmpl.ID, params),
		}
	}
	e.Stats.TemplateHits.Add(1)
	return []wire.Msg{makeTemplateApply(tmpl.ID, params)}
}
```

- [ ] **Step 4: Run existing compress tests to verify nothing is broken**

```bash
go test ./internal/compress/ -v
```

Expected: all existing tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/compress/compress.go
git commit -m "feat: wire Stats counters into Encoder dict and template paths"
```

---

## Task 3 — Encoder registry and `BytesIn`/`BytesOut` tracking

**Files:**
- Modify: `internal/server/server.go` (add `encoders sync.Map` field)
- Modify: `internal/server/session.go` (register encoder; wrap sendFn)

- [ ] **Step 1: Add `encoders sync.Map` to the `Server` struct in `server.go`**

In the `Server` struct definition, add the field after `appState`:

```go
type Server struct {
	displayNum int
	tcpPort    int

	unixL net.Listener
	tcpL  net.Listener

	mu         sync.Mutex
	clientConn net.Conn
	clientW    sync.Mutex
	nextID     atomic.Uint32
	appConns   map[uint32]net.Conn
	appState   map[uint32]*x11.AppConn
	encoders   sync.Map // uint32 connID → *compress.Encoder
}
```

(`sync.Map` is zero-value usable; no initialisation needed in `New`.)

- [ ] **Step 2: Register and unregister the encoder in `relayAppToClient` in `session.go`**

In `relayAppToClient`, after creating `enc`, add registration before the deferred delete. Also wrap `sendFn` with stats tracking. Replace the section from `ac := x11.NewAppConn(...)` through the `drainRequests` call with:

```go
ac := x11.NewAppConn(connID, order)
enc := compress.NewEncoder(connID, 4*1024*1024)
s.mu.Lock()
s.appState[connID] = ac
s.mu.Unlock()

s.encoders.Store(connID, enc)
defer func() {
	s.encoders.Delete(connID)
	s.mu.Lock()
	delete(s.appState, connID)
	s.mu.Unlock()
	enc.OnClientDisconnect()
}()

drainRequests(app, ac, enc, func(data []byte) {
	enc.Stats.BytesIn.Add(int64(len(data)))
	s.sendToClient(connID, data)
	enc.Stats.BytesOut.Add(int64(len(data)))
})
```

- [ ] **Step 3: Run all tests to verify nothing is broken**

```bash
go test ./...
```

Expected: all tests pass.

- [ ] **Step 4: Commit**

```bash
git add internal/server/server.go internal/server/session.go
git commit -m "feat: encoder registry and BytesIn/BytesOut tracking in sendFn"
```

---

## Task 4 — HTTP stats handler

**Files:**
- Create: `internal/server/stats.go`
- Create: `internal/server/stats_test.go`

- [ ] **Step 1: Write the failing tests first (`internal/server/stats_test.go`)**

```go
package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"james.id.au/proxxxy/internal/compress"
)

func TestHandleStats_Full(t *testing.T) {
	s := &Server{}
	enc := compress.NewEncoder(1, 4*1024*1024)
	enc.Stats.BytesIn.Add(1000)
	enc.Stats.BytesOut.Add(800)
	enc.Stats.DictHits.Add(5)
	s.encoders.Store(uint32(1), enc)

	req := httptest.NewRequest("GET", "/stats", nil)
	w := httptest.NewRecorder()
	s.handleStats(w, req)

	var resp statsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.ActiveConnections != 1 {
		t.Errorf("ActiveConnections: got %d want 1", resp.ActiveConnections)
	}
	if resp.Aggregate.BytesIn != 1000 {
		t.Errorf("Aggregate.BytesIn: got %d want 1000", resp.Aggregate.BytesIn)
	}
	if want := 800.0 / 1000.0; resp.Aggregate.Ratio != want {
		t.Errorf("Aggregate.Ratio: got %f want %f", resp.Aggregate.Ratio, want)
	}
	if resp.Connections == nil {
		t.Error("Connections should be present in full response")
	}
	if _, ok := resp.Connections["1"]; !ok {
		t.Error("Connections should have key '1'")
	}
}

func TestHandleStats_AggregateOnly(t *testing.T) {
	s := &Server{}
	enc := compress.NewEncoder(1, 4*1024*1024)
	enc.Stats.BytesIn.Add(500)
	enc.Stats.BytesOut.Add(500)
	s.encoders.Store(uint32(1), enc)

	req := httptest.NewRequest("GET", "/stats?aggregate=1", nil)
	w := httptest.NewRecorder()
	s.handleStats(w, req)

	var resp statsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Connections != nil {
		t.Errorf("Connections should be absent in aggregate response, got %v", resp.Connections)
	}
	if resp.Aggregate.Ratio != 1.0 {
		t.Errorf("Ratio: got %f want 1.0 (no compression while bypassed)", resp.Aggregate.Ratio)
	}
}

func TestHandleStats_Empty(t *testing.T) {
	s := &Server{}

	req := httptest.NewRequest("GET", "/stats", nil)
	w := httptest.NewRecorder()
	s.handleStats(w, req)

	var resp statsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.ActiveConnections != 0 {
		t.Errorf("ActiveConnections: got %d want 0", resp.ActiveConnections)
	}
	if resp.Aggregate.Ratio != 1.0 {
		t.Errorf("Ratio should be 1.0 when no data, got %f", resp.Aggregate.Ratio)
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail (handler not yet written)**

```bash
go test ./internal/server/ -run TestHandleStats -v
```

Expected: compile error or FAIL — `handleStats` and `statsResponse` don't exist yet.

- [ ] **Step 3: Create `internal/server/stats.go`**

```go
package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	"james.id.au/proxxxy/internal/compress"
)

type connStatsJSON struct {
	BytesIn      int64   `json:"bytes_in"`
	BytesOut     int64   `json:"bytes_out"`
	Ratio        float64 `json:"ratio"`
	DictHits     int64   `json:"dict_hits"`
	DictDefines  int64   `json:"dict_defines"`
	DictPasses   int64   `json:"dict_passthroughs"`
	TemplateHits int64   `json:"template_hits"`
	TemplateDefs int64   `json:"template_defines"`
}

type statsResponse struct {
	ClientConnected   bool                     `json:"client_connected"`
	ActiveConnections int                      `json:"active_connections"`
	Aggregate         connStatsJSON            `json:"aggregate"`
	Connections       map[string]connStatsJSON `json:"connections,omitempty"`
}

func snapshotToConn(snap compress.Snapshot) connStatsJSON {
	ratio := 1.0
	if snap.BytesIn > 0 {
		ratio = float64(snap.BytesOut) / float64(snap.BytesIn)
	}
	return connStatsJSON{
		BytesIn:      snap.BytesIn,
		BytesOut:     snap.BytesOut,
		Ratio:        ratio,
		DictHits:     snap.DictHits,
		DictDefines:  snap.DictDefines,
		DictPasses:   snap.DictPasses,
		TemplateHits: snap.TemplateHits,
		TemplateDefs: snap.TemplateDefs,
	}
}

func sumConn(a, b connStatsJSON) connStatsJSON {
	return connStatsJSON{
		BytesIn:      a.BytesIn + b.BytesIn,
		BytesOut:     a.BytesOut + b.BytesOut,
		DictHits:     a.DictHits + b.DictHits,
		DictDefines:  a.DictDefines + b.DictDefines,
		DictPasses:   a.DictPasses + b.DictPasses,
		TemplateHits: a.TemplateHits + b.TemplateHits,
		TemplateDefs: a.TemplateDefs + b.TemplateDefs,
	}
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	aggregateOnly := r.URL.Query().Get("aggregate") == "1"

	s.mu.Lock()
	clientConnected := s.clientConn != nil
	s.mu.Unlock()

	var agg connStatsJSON
	var count int
	conns := make(map[string]connStatsJSON)

	s.encoders.Range(func(key, val any) bool {
		connID := key.(uint32)
		enc := val.(*compress.Encoder)
		cs := snapshotToConn(enc.Stats.Snapshot())
		agg = sumConn(agg, cs)
		count++
		if !aggregateOnly {
			conns[strconv.FormatUint(uint64(connID), 10)] = cs
		}
		return true
	})

	if agg.BytesIn > 0 {
		agg.Ratio = float64(agg.BytesOut) / float64(agg.BytesIn)
	} else {
		agg.Ratio = 1.0
	}

	resp := statsResponse{
		ClientConnected:   clientConnected,
		ActiveConnections: count,
		Aggregate:         agg,
	}
	if !aggregateOnly {
		resp.Connections = conns
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/server/ -run TestHandleStats -v
```

Expected: all three `TestHandleStats_*` tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/server/stats.go internal/server/stats_test.go
git commit -m "feat: HTTP stats handler with per-connection and aggregate JSON"
```

---

## Task 5 — Wire stats HTTP listener into Server; update `New()` signature

**Files:**
- Modify: `internal/server/server.go`
- Modify: `internal/server/server_test.go`
- Modify: `internal/server/e2e_test.go`

- [ ] **Step 1: Add `statsPort` and `statsHTTP` fields; update `New()`, `Start()`, `Stop()`**

In `internal/server/server.go`:

Add the new fields to `Server`:
```go
type Server struct {
	displayNum int
	tcpPort    int
	statsPort  int

	unixL     net.Listener
	tcpL      net.Listener
	statsHTTP *http.Server

	mu         sync.Mutex
	clientConn net.Conn
	clientW    sync.Mutex
	nextID     atomic.Uint32
	appConns   map[uint32]net.Conn
	appState   map[uint32]*x11.AppConn
	encoders   sync.Map
}
```

Update `New()`:
```go
func New(displayNum, tcpPort, statsPort int) *Server {
	return &Server{
		displayNum: displayNum,
		tcpPort:    tcpPort,
		statsPort:  statsPort,
		appConns:   make(map[uint32]net.Conn),
		appState:   make(map[uint32]*x11.AppConn),
	}
}
```

Add `startStatsHTTP()` method and call it at the end of `Start()` (after the goroutines are launched):
```go
func (s *Server) startStatsHTTP() {
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", s.handleStats)
	s.statsHTTP = &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", s.statsPort),
		Handler: mux,
	}
	go func() {
		if err := s.statsHTTP.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Println("server: stats HTTP:", err)
		}
	}()
}
```

In `Start()`, add before `return nil`:
```go
s.startStatsHTTP()
```

In `Stop()`, add before removing sockets:
```go
if s.statsHTTP != nil {
	s.statsHTTP.Close()
}
```

Add `"net/http"` to the import block in `server.go`.

- [ ] **Step 2: Update `New()` call sites in test files**

In `internal/server/server_test.go`, update both calls:
```go
// TestHandshakeVersionMismatch
s := server.New(99, 17199, 17299)

// TestHandshakeSuccess
s := server.New(99, 17200, 17300)
```

In `internal/server/e2e_test.go`, update the call:
```go
s := server.New(97, 17197, 17297)
```

- [ ] **Step 3: Run all tests**

```bash
go test ./...
```

Expected: all tests pass.

- [ ] **Step 4: Commit**

```bash
git add internal/server/server.go internal/server/server_test.go internal/server/e2e_test.go
git commit -m "feat: wire stats HTTP listener into Server; update New() to accept statsPort"
```

---

## Task 6 — Add `-stats-port` flag to `cmd/server/main.go`

**Files:**
- Modify: `cmd/server/main.go`

- [ ] **Step 1: Add the flag and pass stats port to `New()`**

Replace the body of `main()` in `cmd/server/main.go`:

```go
func main() {
	display   := flag.Int("display",    2,    "X display number to present (e.g. 2 → DISPLAY=:2)")
	port      := flag.Int("port",       7100, "TCP port for proxxxy-client")
	statsPort := flag.Int("stats-port", 0,    "HTTP stats port (0 = port+1)")
	flag.Parse()

	sp := *statsPort
	if sp == 0 {
		sp = *port + 1
	}

	s := server.New(*display, *port, sp)
	if err := s.Start(); err != nil {
		log.Fatal(err)
	}
	log.Printf("DISPLAY=:%d  client-port=%d  stats=http://localhost:%d/stats",
		*display, *port, sp)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	s.Stop()
}
```

- [ ] **Step 2: Build to verify it compiles**

```bash
go build ./cmd/server/
```

Expected: no errors.

- [ ] **Step 3: Run all tests**

```bash
go test ./...
```

Expected: all pass.

- [ ] **Step 4: Commit**

```bash
git add cmd/server/main.go
git commit -m "feat: add -stats-port flag to proxxxy-server (default port+1)"
```

---

## Task 7 — Rewrite `cmd/ctl/main.go`

**Files:**
- Rewrite: `cmd/ctl/main.go`

- [ ] **Step 1: Rewrite `cmd/ctl/main.go`**

```go
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
)

func main() {
	server    := flag.String("server",    "http://localhost:7101", "proxxxy stats server base URL")
	aggregate := flag.Bool("aggregate",   false,                   "show aggregate stats only")
	flag.Parse()

	url := *server + "/stats"
	if *aggregate {
		url += "?aggregate=1"
	}

	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatal(err)
	}

	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		log.Fatal(err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
```

- [ ] **Step 2: Build to verify it compiles**

```bash
go build -o /tmp/proxxxy-ctl ./cmd/ctl/
```

Expected: no errors, binary at `/tmp/proxxxy-ctl`.

- [ ] **Step 3: Run all tests**

```bash
go test ./...
```

Expected: all pass.

- [ ] **Step 4: Commit**

```bash
git add cmd/ctl/main.go
git commit -m "feat: rewrite proxxxy-ctl as HTTP client with -aggregate flag"
```

---

## Task 8 — Build everything and run xterm end-to-end

**Goal:** Get xterm rendering on display `:2` (proxied to `:1`). Take a screenshot to confirm.

**Files:** No code changes expected; fix any issues found.

- [ ] **Step 1: Verify Xorg :1 is running**

```bash
DISPLAY=:1 xdpyinfo | head -5
```

Expected: shows server vendor, protocol version. If this fails, Xorg :1 is not running — start it with `Xorg :1 -config /etc/X11/xorg-dummy.conf &` or equivalent.

- [ ] **Step 2: Build all binaries**

```bash
go build -o /tmp/proxxxy-server   ./cmd/server/ && \
go build -o /tmp/proxxxy-client   ./cmd/client/ && \
go build -o /tmp/proxxxy-ctl      ./cmd/ctl/
```

Expected: no errors.

- [ ] **Step 3: Clean up stale sockets and start the stack**

```bash
rm -f /tmp/.X11-unix/X2 /tmp/.X2-lock
DISPLAY=:1 /tmp/proxxxy-server -display 2 -port 7100 &
sleep 0.5
DISPLAY=:1 /tmp/proxxxy-client -server localhost:7100 &
sleep 0.5
```

Expected: server log shows `DISPLAY=:2 client-port=7100 stats=http://localhost:7101/stats`; client log shows `client: live`.

- [ ] **Step 4: Confirm stats endpoint is up**

```bash
/tmp/proxxxy-ctl
```

Expected: JSON with `"client_connected": true`, `"active_connections": 0`.

- [ ] **Step 5: Launch xterm on the proxied display**

```bash
DISPLAY=:2 xterm &
sleep 1
```

Watch server and client logs for connection messages.

- [ ] **Step 6: Take a screenshot and view it**

```bash
DISPLAY=:1 scrot /tmp/xterm-screenshot.png
```

Then view `/tmp/xterm-screenshot.png`. Confirm xterm window is visible with a shell prompt. If `scrot` is not installed: `DISPLAY=:1 import -window root /tmp/xterm-screenshot.png`.

- [ ] **Step 7: If xterm does not appear — diagnose**

Check for errors in server/client logs. Common causes and fixes:

**"connection refused" on client dial** — client started before server was ready. Kill and restart with longer sleep.

**xterm exits immediately with "can't open display :2"** — server's Unix socket not created. Check `ls /tmp/.X11-unix/X2`.

**xterm hangs after connection setup** — the setup response isn't arriving back to xterm. This means the reverse relay (real-display → client → server → xterm) has a problem. Add `log.Printf("readFromClient: got %d bytes for conn %d", len(data), connID)` to `readFromClient` in `server.go` to confirm data flows.

**ProcessRequest panics** — add `defer func() { if r := recover(); r != nil { log.Println("ProcessRequest panic:", r) } }()` temporarily in `ProcessRequest` to catch it.

- [ ] **Step 8: Check compression stats with xterm running**

```bash
/tmp/proxxxy-ctl
```

Expected: `"active_connections": 1` (or more), BytesIn/BytesOut non-zero and equal (bypass mode, ratio = 1.0).

- [ ] **Step 9: Kill the stack cleanly**

```bash
kill %3 %2 %1 2>/dev/null; wait
```

- [ ] **Step 10: Commit any fixes made in this task**

If any code was changed:
```bash
git add -p
git commit -m "fix: <describe what broke and how you fixed it>"
```

---

## Task 9 — Test xterm interactivity

**Goal:** Verify keyboard input reaches xterm and output is rendered. Use `xdotool` to inject keystrokes without a physical keyboard.

- [ ] **Step 1: Start the full stack again**

```bash
rm -f /tmp/.X11-unix/X2 /tmp/.X2-lock
DISPLAY=:1 /tmp/proxxxy-server -display 2 -port 7100 &
sleep 0.5
DISPLAY=:1 /tmp/proxxxy-client -server localhost:7100 &
sleep 0.5
DISPLAY=:2 xterm &
sleep 1
```

- [ ] **Step 2: Find the xterm window ID on display :1**

```bash
DISPLAY=:1 xdotool search --class XTerm
```

Expected: one or more window IDs printed. Note the ID (e.g. `12345678`).

- [ ] **Step 3: Give xterm focus and type a command**

```bash
DISPLAY=:1 xdotool windowfocus <window-id>
sleep 0.2
DISPLAY=:1 xdotool type --window <window-id> 'echo proxy_works'
DISPLAY=:1 xdotool key --window <window-id> Return
sleep 0.5
```

- [ ] **Step 4: Take a screenshot and verify output**

```bash
DISPLAY=:1 scrot /tmp/xterm-interactive.png
```

View `/tmp/xterm-interactive.png`. Confirm `echo proxy_works` and its output `proxy_works` appear in the xterm window.

- [ ] **Step 5: If input is not echoed — diagnose event relay**

Key events from display `:1` flow: Xorg-`:1` → `relayXToServer` goroutine in client → `MsgX11Data` → `readFromClient` in server → xterm's socket.

Add temporary logging to `readFromClient` in `server.go`:
```go
func (s *Server) readFromClient(conn net.Conn) {
    for {
        msg, err := wire.Read(conn)
        if err != nil {
            return
        }
        if msg.Type != wire.MsgX11Data {
            continue
        }
        connID, data, err := wire.ParseX11Data(msg.Payload)
        if err != nil {
            continue
        }
        log.Printf("readFromClient: connID=%d bytes=%d", connID, len(data))  // temporary
        s.mu.Lock()
        app := s.appConns[connID]
        s.mu.Unlock()
        if app != nil {
            if _, werr := app.Write(data); werr != nil {
                log.Println("server: write to app:", werr)
            }
        }
    }
}
```

If no log lines appear when typing: the client's `relayXToServer` is not sending data. Verify xterm's real-display connection is in `c.xConns`.

- [ ] **Step 6: Run full test suite to confirm nothing regressed**

```bash
go test ./...
```

Expected: all pass.

- [ ] **Step 7: Commit any fixes and clean up temporary logging**

```bash
git add -p
git commit -m "fix: <describe interactivity fix>"
```

---

## Watch Commands (for reference)

Once the stack is running:

```bash
# Full stats (aggregate + per-connection breakdown)
watch -n1 /tmp/proxxxy-ctl

# Aggregate only (compact, fixed-size terminal view)
watch -n1 '/tmp/proxxxy-ctl -aggregate'

# Custom server URL
watch -n1 '/tmp/proxxxy-ctl -server http://localhost:7101'
```

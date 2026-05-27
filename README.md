# proxxxy

A resumable X11 proxy. Lets X11 sessions survive network interruptions — apps keep running and windows reappear after reconnect without losing state.

```
X11 app → fake display (:N) → proxxxy-server → TCP → proxxxy-client → real display
```

When the client disconnects and reconnects, the server synthesises the full X11 state (windows, pixmaps, GCs, cursors, fonts) onto a fresh connection so the app never notices the gap.

## Status

Phase 1 (opaque relay) and Phase 2 (state synthesis on reconnect) are complete and tested against GIMP, Firefox, Chromium, and LibreOffice. Phase 3 (delta compression) is implemented but not yet wired in — the client has no decoder yet.

## Building

### Prerequisites

**Linux**
```bash
sudo apt install golang git xvfb
# Go 1.24+ required; check with: go version
```

**macOS**
```bash
brew install go git
# Go 1.24+ required; check with: go version
```

> **Note:** proxxxy-server and proxxxy-client both speak to X11 displays. On macOS, the real display side requires [XQuartz](https://www.xquartz.org/) (provides a DISPLAY for the client to connect to). The server side (fake display) must run on Linux, since it creates a Unix socket in `/tmp/.X11-unix/`.

### Build

```bash
git clone https://github.com/j4m3z0r/proxxxy.git
cd proxxxy
make
```

This builds `proxxxy-server`, `proxxxy-client`, `proxxxy-testclient`, `proxxxy-ctl`, and `proxxxy-xlog` in the current directory.

Individual targets: `make build` (default), `make test`, `make install` (installs to `$GOBIN`), `make clean`.

### Run tests

```bash
make test
```

The server package includes an E2E reconnect test that spins up Xvfb on `:97`. On Linux with `xvfb-run` installed:
```bash
sudo apt install xvfb
go test ./internal/server/ -v -run TestReconnect
```

## Running

### Quick start (Linux)

You need two displays: a real one (`:1`) and the fake proxy display (`:2`).

```bash
# Start the server (creates fake display :2, listens on TCP port 7100)
rm -f /tmp/.X11-unix/X2 /tmp/.X2-lock
./proxxxy-server -display 2 -port 7100 &

# Connect the client (real display :1, connects to server)
DISPLAY=:1 ./proxxxy-client -server localhost:7100 &

# Launch an app on the fake display
DISPLAY=:2 xterm &
```

### Reconnect test

```bash
# Kill and restart the client to simulate a network interruption
pkill -f proxxxy-client
DISPLAY=:1 ./proxxxy-client -server localhost:7100 &
# App windows reappear; state is preserved
```

### GIMP (full synthesis path)

GIMP exercises the complete synthesis path: MIT-SHM, RENDER extension, SYNC counters, depth-32 ARGB windows, and complex window hierarchies.

```bash
DISPLAY=:2 gimp &
# Reconnect test: kill client, relaunch — GIMP windows should reappear intact
pkill -f proxxxy-client
DISPLAY=:1 ./proxxxy-client -server localhost:7100 &
```

## Automated reconnect harness

`tests/run_stage.sh` drives full reconnect cycles against real apps on Xvfb and checks for X11 errors. Requires Linux with `xvfb-run`, `xdotool`, and the relevant apps installed.

```bash
# Run 3 reconnect cycles against LibreOffice (stage 9)
tests/run_stage.sh --stage 9 --reconnects 3
# Results in tests/results/YYYYMMDD-HHMMSS-stage9/
```

| Stage | App | What it tests |
|---|---|---|
| 1 | testclient | Baseline: minimal window, pixmap redraws |
| 2–6 | GIMP | Full synthesis: SHM, RENDER, depth-32 ARGB, complex hierarchies |
| 7 | Firefox | Browser rendering, URL navigation |
| 8 | Chromium | Browser rendering + ridBase-change event remap |
| 9 | LibreOffice | Format menu interaction |

## Architecture

See [`CLAUDE.md`](CLAUDE.md) for a detailed architecture reference including the synthesis sequence, ID remapping, known gaps, and protocol documentation.

## Flags

**proxxxy-server**

| Flag | Default | Description |
|---|---|---|
| `-display N` | `2` | Fake X display number to create (`:N`) |
| `-port P` | `7100` | TCP port to listen on for clients |

**proxxxy-client**

| Flag | Default | Description |
|---|---|---|
| `-server host:port` | `localhost:7100` | Address of proxxxy-server |

## Known limitations

- **GPU acceleration unavailable**: DRI2/DRI3/Present extensions are suppressed so apps fall back to software GL (Mesa llvmpipe, SwiftShader). WebGL works but uses CPU rendering.
- **Phase 3 (delta compression) not yet active**: client decoder not yet implemented; all traffic is relayed as raw X11.
- **TCP only**: the server-client link is unencrypted TCP. For remote use, tunnel through SSH (`-L 7100:localhost:7100`).
- **Linux server required**: the fake display is a Unix socket in `/tmp/.X11-unix/`; this path is Linux-specific.

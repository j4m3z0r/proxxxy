#!/usr/bin/env bash
# E2E reconnect harness for proxxxy.
# Usage: tests/run_stage.sh --stage N [--reconnects N] [--display N]
# Stages: 1=testclient 2=xclock 3=xterm 4=mousepad 5=gimp-baseline 6=gimp 7=firefox 8=chromium 9=libreoffice
# Requirements: Xvfb, scrot, xdotool in PATH.

set -euo pipefail

STAGE=1
RECONNECTS=10
BASE_DISP=90

while [[ $# -gt 0 ]]; do
    case $1 in
        --stage)      STAGE=$2;      shift 2 ;;
        --reconnects) RECONNECTS=$2; shift 2 ;;
        --display)    BASE_DISP=$2;  shift 2 ;;
        *) echo "unknown arg: $1" >&2; exit 1 ;;
    esac
done

REAL_DISP=$BASE_DISP
FAKE_DISP=$((BASE_DISP + 1))
PORT=$((7000 + BASE_DISP))
REPO="$(cd "$(dirname "$0")/.." && pwd)"
RESULTS="$REPO/tests/results/$(date +%Y%m%d-%H%M%S)-stage${STAGE}"
mkdir -p "$RESULTS"

SERVER_PID="" CLIENT_PID="" APP_PID="" XVFB_PID="" XCLOCK_ANCHOR_PID=""

cleanup() {
    [[ -n "${APP_PID:-}" ]]           && kill "$APP_PID"           2>/dev/null || true
    # LibreOffice spawns soffice.bin as a separate process; kill it explicitly.
    [[ "${STAGE:-0}" -eq 9 ]]         && pkill -x soffice.bin      2>/dev/null || true
    # Firefox leaves .parentlock on kill; remove it so the next run starts cleanly.
    [[ "${STAGE:-0}" -eq 7 ]]         && find "$HOME/.mozilla/firefox" -name ".parentlock" -delete 2>/dev/null || true
    [[ -n "${CLIENT_PID:-}" ]]        && kill "$CLIENT_PID"        2>/dev/null || true
    [[ -n "${SERVER_PID:-}" ]]        && kill "$SERVER_PID"        2>/dev/null || true
    [[ -n "${XCLOCK_ANCHOR_PID:-}" ]] && kill "$XCLOCK_ANCHOR_PID" 2>/dev/null || true
    [[ -n "${XVFB_PID:-}" ]]         && kill "$XVFB_PID"          2>/dev/null || true
    rm -f "/tmp/.X11-unix/X${FAKE_DISP}" "/tmp/.X${FAKE_DISP}-lock"
}
trap cleanup EXIT

log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { log "FAIL: $*"; log "Results saved to: $RESULTS"; exit 1; }

log "Building binaries..."
go build -o "$RESULTS/proxxxy-server" "$REPO/cmd/server/"
go build -o "$RESULTS/proxxxy-client" "$REPO/cmd/client/"

log "Starting Xvfb :${REAL_DISP}..."
Xvfb ":${REAL_DISP}" -screen 0 1920x1080x24 -ac &
XVFB_PID=$!
sleep 0.5
# Anchor client: keeps Xvfb alive (and its atom table intact) across proxxxy-client
# reconnects.  Without this, Xvfb resets all user-defined atoms the moment its last
# client disconnects — which happens during each reconnect cycle.  On a real Xorg
# display the WM/desktop fills this role; in the test environment xclock does it.
DISPLAY=":${REAL_DISP}" xclock &
XCLOCK_ANCHOR_PID=$!

start_server() {
    rm -f "/tmp/.X11-unix/X${FAKE_DISP}" "/tmp/.X${FAKE_DISP}-lock"
    DISPLAY=":${REAL_DISP}" "$RESULTS/proxxxy-server" \
        -display "${FAKE_DISP}" -port "${PORT}" \
        >> "$RESULTS/server.log" 2>&1 &
    SERVER_PID=$!
    sleep 0.3
}

start_client() {
    DISPLAY=":${REAL_DISP}" "$RESULTS/proxxxy-client" \
        -server "localhost:${PORT}" \
        >> "$RESULTS/client.log" 2>&1 &
    CLIENT_PID=$!
}

wait_for_window() {
    local title="$1" deadline=$((SECONDS + 30))
    log "Waiting for window: '$title' (search=${APP_SEARCH:-name})..."
    while [[ $SECONDS -lt $deadline ]]; do
        if DISPLAY=":${REAL_DISP}" xdotool search "--${APP_SEARCH:-name}" "$title" >/dev/null 2>&1; then
            log "Window found: '$title'"
            return 0
        fi
        sleep 0.5
    done
    fail "window '$title' never appeared (30s timeout)"
}

wait_for_synthesis() {
    local since="$1" deadline=$((SECONDS + 30))
    log "Waiting for synthesis (server.log line >= ${since})..."
    while [[ $SECONDS -lt $deadline ]]; do
        if tail -n "+${since}" "$RESULTS/server.log" 2>/dev/null \
                | grep -q "server: synthesis complete"; then
            sleep 0.3
            log "Synthesis complete."
            return 0
        fi
        sleep 0.2
    done
    fail "synthesis never completed after line ${since} (30s timeout)"
}

screenshot() {
    local name="$1"
    DISPLAY=":${REAL_DISP}" scrot "$RESULTS/${name}.png" 2>/dev/null \
        && log "  screenshot: ${name}.png" \
        || log "  screenshot failed (non-fatal): ${name}.png"
}

ERROR_OFFSET=1
check_new_errors() {
    local errs
    # Exclude synthesis/barrier-phase errors — they are discarded by synthRelay and don't reach the app.
    errs=$(tail -n "+${ERROR_OFFSET}" "$RESULTS/client.log" 2>/dev/null \
        | grep -v "X error during synthesis\|X error during barrier" \
        | grep -E "BadWindow|BadMatch|BadFont|BadGC|BadValue|BadRequest|code=[1-9]" || true)
    ERROR_OFFSET=$(( $(wc -l < "$RESULTS/client.log" 2>/dev/null || echo 1) + 1 ))
    printf '%s' "$errs"
}

send_input() {
    local mode="$1" cycle="$2"
    case $mode in
        none)
            sleep 0.5
            ;;
        xterm)
            local wid
            wid=$(DISPLAY=":${REAL_DISP}" xdotool search --class "XTerm" 2>/dev/null | head -1 || true)
            if [[ -n "$wid" ]]; then
                DISPLAY=":${REAL_DISP}" xdotool type --window "$wid" "echo hello${cycle}"
                DISPLAY=":${REAL_DISP}" xdotool key --window "$wid" Return
                sleep 0.3
            else
                log "  xterm window not found for input"
            fi
            ;;
        mousepad)
            local wid
            wid=$(DISPLAY=":${REAL_DISP}" xdotool search --name "Mousepad" 2>/dev/null | head -1 || true)
            if [[ -n "$wid" ]]; then
                DISPLAY=":${REAL_DISP}" xdotool key --window "$wid" alt+F
                sleep 0.3
                DISPLAY=":${REAL_DISP}" xdotool key --window "$wid" Escape
                sleep 0.2
            else
                log "  Mousepad window not found for input"
            fi
            ;;
        gimp)
            local wid wx wy before_n after_n
            # Find the main GIMP window as the largest Gimp-class window by area.
            # Searching by WM_NAME is unreliable: GIMP sets WM_NAME on multiple
            # windows (including tiny stubs), and after reconnect+synthesis the
            # order is unpredictable. The main window (~800×600) always dominates
            # helper windows (1×1, 10×10) in area.
            local max_area=0 w geom gx gy gw gh ga
            for w in $(DISPLAY=":${REAL_DISP}" xdotool search --class "Gimp" 2>/dev/null || true); do
                geom=$(DISPLAY=":${REAL_DISP}" xdotool getwindowgeometry --shell "$w" 2>/dev/null || true)
                gw=$(echo "$geom" | grep '^WIDTH='  | cut -d= -f2)
                gh=$(echo "$geom" | grep '^HEIGHT=' | cut -d= -f2)
                ga=$(( ${gw:-0} * ${gh:-0} ))
                if [[ $ga -gt $max_area ]]; then
                    max_area=$ga; wid=$w
                    gx=$(echo "$geom" | grep '^X=' | cut -d= -f2)
                    gy=$(echo "$geom" | grep '^Y=' | cut -d= -f2)
                    wx=${gx:-0}; wy=${gy:-0}
                fi
            done
            if [[ -n "$wid" ]]; then
                # Count only visible (mapped) windows: the synthesized state may include
                # unmapped popup windows from the previous cycle. When the menu opens it
                # maps a pre-existing window, so --onlyvisible correctly detects the change.
                before_n=$(DISPLAY=":${REAL_DISP}" xdotool search --onlyvisible --class "Gimp" 2>/dev/null | wc -l)
                log "  DEBUG gimp send_input: cycle=${cycle} wid=${wid} wx=${wx:-0} wy=${wy:-0} click=$((${wx:-0}+50)),$((${wy:-0}+15)) before_n=${before_n}"
                # Real pointer click on File menu — exercises XI2 event routing (not XSendEvent)
                DISPLAY=":${REAL_DISP}" xdotool mousemove --sync "$((wx + 50))" "$((wy + 15))"
                DISPLAY=":${REAL_DISP}" xdotool click 1
                sleep 2
                after_n=$(DISPLAY=":${REAL_DISP}" xdotool search --onlyvisible --class "Gimp" 2>/dev/null | wc -l)
                if [[ $after_n -le $before_n ]]; then
                    fail "cycle ${cycle}: GIMP click did not open menu (window count: ${before_n} → ${after_n}; XI2 event routing failure)"
                fi
                DISPLAY=":${REAL_DISP}" xdotool key Escape
                sleep 0.2
            else
                log "  GIMP main window not found for input"
            fi
            ;;
        firefox|chromium)
            local app_class
            local wid wx wy ww wh max_area=0 w geom gx gy gw gh ga
            [[ "$mode" == "firefox" ]] && app_class="firefox-esr" || app_class="Chromium"
            # Find the main browser window as the largest of its class.
            for w in $(DISPLAY=":${REAL_DISP}" xdotool search --onlyvisible --class "$app_class" 2>/dev/null || true); do
                geom=$(DISPLAY=":${REAL_DISP}" xdotool getwindowgeometry --shell "$w" 2>/dev/null || true)
                gw=$(echo "$geom" | grep '^WIDTH='  | cut -d= -f2)
                gh=$(echo "$geom" | grep '^HEIGHT=' | cut -d= -f2)
                ga=$(( ${gw:-0} * ${gh:-0} ))
                if [[ $ga -gt $max_area ]]; then
                    max_area=$ga; wid=$w; ww=${gw:-0}; wh=${gh:-0}
                    gx=$(echo "$geom" | grep '^X=' | cut -d= -f2)
                    gy=$(echo "$geom" | grep '^Y=' | cut -d= -f2)
                    wx=${gx:-0}; wy=${gy:-0}
                fi
            done
            if [[ -n "$wid" ]]; then
                local before_title after_title
                before_title=$(DISPLAY=":${REAL_DISP}" xdotool getwindowname "$wid" 2>/dev/null || true)
                log "  DEBUG browser send_input: cycle=${cycle} class=${app_class} wid=${wid} ${ww}x${wh}+${wx}+${wy} title='${before_title}'"
                # Focus the window explicitly on the real display.
                DISPLAY=":${REAL_DISP}" xdotool mousemove --sync "$((wx + ww/2))" "$((wy + wh/2))"
                DISPLAY=":${REAL_DISP}" xdotool click 1
                sleep 0.3
                DISPLAY=":${REAL_DISP}" xdotool windowfocus --sync "$wid"
                # Navigate to a URL with a distinct title via the address bar.
                # Choose a target URL whose title will differ from before_title so
                # a same-URL re-navigation never falsely appears as "no change".
                local nav_url
                if [[ "$before_title" == *"About About"* ]]; then
                    nav_url="about:blank"
                else
                    nav_url="about:about"
                fi
                DISPLAY=":${REAL_DISP}" xdotool key --window "$wid" ctrl+l
                sleep 0.5
                DISPLAY=":${REAL_DISP}" xdotool type --window "$wid" --clearmodifiers "$nav_url"
                sleep 0.3
                DISPLAY=":${REAL_DISP}" xdotool key --window "$wid" Return
                # Poll up to 15s for the title to change.
                local deadline=$((SECONDS + 15))
                after_title="$before_title"
                while [[ $SECONDS -lt $deadline && "$after_title" == "$before_title" ]]; do
                    sleep 0.5
                    after_title=$(DISPLAY=":${REAL_DISP}" xdotool getwindowname "$wid" 2>/dev/null || true)
                done
                if [[ "$after_title" == "$before_title" ]]; then
                    fail "cycle ${cycle}: ${app_class} URL navigation did not change title ('${before_title}'; keyboard routing failure)"
                fi
                log "  browser title changed: '${before_title}' → '${after_title}'"
            else
                log "  ${app_class} main window not found for input"
            fi
            ;;
        libreoffice)
            local wid wx wy ww wh before_n after_n
            local max_area=0 w geom gx gy gw gh ga
            # Find the main LibreOffice Writer window as the largest libreoffice-writer window.
            for w in $(DISPLAY=":${REAL_DISP}" xdotool search --onlyvisible --class "libreoffice-writer" 2>/dev/null || true); do
                geom=$(DISPLAY=":${REAL_DISP}" xdotool getwindowgeometry --shell "$w" 2>/dev/null || true)
                gw=$(echo "$geom" | grep '^WIDTH='  | cut -d= -f2)
                gh=$(echo "$geom" | grep '^HEIGHT=' | cut -d= -f2)
                ga=$(( ${gw:-0} * ${gh:-0} ))
                if [[ $ga -gt $max_area ]]; then
                    max_area=$ga; wid=$w; ww=${gw:-0}; wh=${gh:-0}
                    gx=$(echo "$geom" | grep '^X=' | cut -d= -f2)
                    gy=$(echo "$geom" | grep '^Y=' | cut -d= -f2)
                    wx=${gx:-0}; wy=${gy:-0}
                fi
            done
            if [[ -n "$wid" ]]; then
                # Count all visible windows to detect menu popup creation.
                # VCL popup menus map a pre-existing (unmapped) X window, so
                # --onlyvisible catches the transition.
                before_n=$(DISPLAY=":${REAL_DISP}" xdotool search --onlyvisible 2>/dev/null | wc -l)
                log "  DEBUG libreoffice send_input: cycle=${cycle} wid=${wid} ${ww}x${wh}+${wx}+${wy} before_n=${before_n}"
                # Click in the document editing area and type text.
                DISPLAY=":${REAL_DISP}" xdotool mousemove --sync "$((wx + ww/2))" "$((wy + wh*2/3))"
                DISPLAY=":${REAL_DISP}" xdotool click 1
                sleep 0.2
                DISPLAY=":${REAL_DISP}" xdotool type "hello${cycle}"
                DISPLAY=":${REAL_DISP}" xdotool key Return
                sleep 0.3
                # Open Format menu via XTest to verify keyboard event routing.
                DISPLAY=":${REAL_DISP}" xdotool key alt+F
                sleep 2
                after_n=$(DISPLAY=":${REAL_DISP}" xdotool search --onlyvisible 2>/dev/null | wc -l)
                if [[ $after_n -le $before_n ]]; then
                    fail "cycle ${cycle}: LibreOffice menu did not open (window count: ${before_n} → ${after_n}; keyboard routing failure)"
                fi
                DISPLAY=":${REAL_DISP}" xdotool key Escape
                sleep 0.3
            else
                log "  LibreOffice Writer window not found for input"
            fi
            ;;
    esac
}

# Stage 5: GIMP baseline (no proxxxy)
if [[ $STAGE -eq 5 ]]; then
    log "Stage 5: GIMP baseline — running GIMP directly on :${REAL_DISP} (no proxxxy)"
    GDK_SYNCHRONIZE=1 DISPLAY=":${REAL_DISP}" gimp \
        >> "$RESULTS/app.log" 2>&1 &
    APP_PID=$!
    log "Waiting 15s for GIMP to start and settle..."
    sleep 15
    log "Capturing baseline errors..."
    grep -E "BadWindow|BadMatch|BadFont|BadGC|BadValue|BadRequest|code=[1-9]" \
        "$RESULTS/app.log" 2>/dev/null | tee "$RESULTS/baseline-errors.txt" || true
    log "Stage 5 baseline captured: $RESULTS/baseline-errors.txt"
    exit 0
fi

# Stage config
APP_SEARCH="name"
APP_SETTLE=0            # extra seconds after window appears before first cycle
POST_RECONNECT_SETTLE=0 # extra seconds after synthesis before taking post-reconnect screenshot
case $STAGE in
    1)
        go build -o "$RESULTS/testclient" "$REPO/cmd/testclient/"
        APP_BIN="$RESULTS/testclient"
        APP_ARGS=()
        APP_ENV=()
        APP_WINDOW="proxxxy testclient"
        INPUT_MODE="none"
        ;;
    2)
        APP_BIN="xclock"
        APP_ARGS=()
        APP_ENV=(XSYNCHRONIZE=yes)
        APP_WINDOW="xclock"
        INPUT_MODE="none"
        ;;
    3)
        APP_BIN="xterm"
        APP_ARGS=(-synchronous)
        APP_ENV=()
        APP_WINDOW="XTerm"
        APP_SEARCH="class"
        INPUT_MODE="xterm"
        ;;
    4)
        APP_BIN="mousepad"
        APP_ARGS=()
        APP_ENV=(GDK_SYNCHRONIZE=1)
        APP_WINDOW="Mousepad"
        INPUT_MODE="mousepad"
        ;;
    6)
        APP_BIN="gimp"
        APP_ARGS=()
        APP_ENV=(GDK_SYNCHRONIZE=1)
        APP_WINDOW="Gimp"
        APP_SEARCH="class"
        INPUT_MODE="gimp"
        # GIMP replaces its splash window with the main window hierarchy a few
        # seconds after the initial window appears.  Waiting here ensures
        # synthesis captures a stable, fully-initialized state rather than an
        # in-progress window replacement.
        APP_SETTLE=8
        ;;
    7)
        # Kill any stale .parentlock files left by a previously killed Firefox
        # instance so the new run doesn't hit the "profile cannot be loaded" dialog.
        find "$HOME/.mozilla/firefox" -name ".parentlock" -delete 2>/dev/null || true
        APP_BIN="firefox"
        APP_ARGS=(--no-remote http://example.com)
        APP_ENV=(GDK_SYNCHRONIZE=1)
        APP_WINDOW="firefox-esr"
        APP_SEARCH="class"
        INPUT_MODE="firefox"
        APP_SETTLE=15
        POST_RECONNECT_SETTLE=20
        ;;
    8)
        APP_BIN="chromium"
        # --user-data-dir: isolated per-run profile.  --no-sandbox: required in
        # virtualized environments without user namespaces.
        APP_ARGS=(--user-data-dir="$RESULTS/cr-profile" --no-sandbox --disable-gpu
                  --no-first-run --disable-extensions --disable-sync
                  http://example.com)
        APP_ENV=()
        APP_WINDOW="Chromium"
        APP_SEARCH="class"
        INPUT_MODE="chromium"
        APP_SETTLE=15
        ;;
    9)
        APP_BIN="libreoffice"
        APP_ARGS=(--writer --norestore --nologo)
        APP_ENV=(GDK_SYNCHRONIZE=1 SAL_USE_VCLPLUGIN=gtk3)
        APP_WINDOW="libreoffice-writer"
        APP_SEARCH="class"
        INPUT_MODE="libreoffice"
        APP_SETTLE=8
        ;;
    *)
        echo "Unknown stage: $STAGE" >&2; exit 1 ;;
esac

start_app() {
    env DISPLAY=":${FAKE_DISP}" "${APP_ENV[@]+"${APP_ENV[@]}"}" \
        "$APP_BIN" "${APP_ARGS[@]+"${APP_ARGS[@]}"}" \
        >> "$RESULTS/app.log" 2>&1 &
    APP_PID=$!
}

# === Main execution ===
log "Stage $STAGE | $RECONNECTS reconnects | real=:${REAL_DISP} fake=:${FAKE_DISP} port=${PORT}"

start_server
start_client
sleep 0.3
start_app

wait_for_window "$APP_WINDOW"
sleep 1
[[ $APP_SETTLE -gt 0 ]] && { log "Settling for ${APP_SETTLE}s..."; sleep "$APP_SETTLE"; }
screenshot "initial"

PASS=0
for CYCLE in $(seq 1 "$RECONNECTS"); do
    log "=== Cycle ${CYCLE}/${RECONNECTS} ==="

    screenshot "cycle-${CYCLE}-pre-input"
    send_input "$INPUT_MODE" "$CYCLE"
    screenshot "cycle-${CYCLE}-post-input"

    ERRS=$(check_new_errors)
    if [[ -n "$ERRS" ]]; then
        log "X11 errors detected:"
        echo "$ERRS"
        fail "X11 errors at cycle ${CYCLE} (see above)"
    fi

    # Note position in server.log before reconnect so wait_for_synthesis
    # only looks at lines added after the reconnect.
    SYNTH_START=$(( $(wc -l < "$RESULTS/server.log" 2>/dev/null || echo 0) + 1 ))

    log "Killing client..."
    kill "$CLIENT_PID" 2>/dev/null || true
    CLIENT_PID=""
    sleep 1

    log "Reconnecting client..."
    start_client
    wait_for_synthesis "$SYNTH_START"
    wait_for_window "$APP_WINDOW"
    sleep 0.5
    if [[ $POST_RECONNECT_SETTLE -gt 0 ]]; then
        log "Post-reconnect settle: ${POST_RECONNECT_SETTLE}s..."
        sleep "$POST_RECONNECT_SETTLE"
    fi

    # Verify input works after reconnect — exercises synthesis correctness.
    screenshot "cycle-${CYCLE}-post-reconnect"
    send_input "$INPUT_MODE" "${CYCLE}-post"
    screenshot "cycle-${CYCLE}-post-reconnect-input"

    ERRS=$(check_new_errors)
    if [[ -n "$ERRS" ]]; then
        log "X11 errors after reconnect cycle ${CYCLE}:"
        echo "$ERRS"
        fail "X11 errors after reconnect at cycle ${CYCLE} (see above)"
    fi

    PASS=$((PASS + 1))
done

log "PASS: Stage ${STAGE} — ${PASS}/${RECONNECTS} reconnects clean"
log "Screenshots and logs: $RESULTS"
exit 0

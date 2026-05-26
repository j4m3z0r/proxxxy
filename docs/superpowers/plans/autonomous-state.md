# proxxxy Autonomous Iteration State

## Current Stage
Stage 2: xclock

## Stage Status
| Stage | App | Status | Date | Notes |
|---|---|---|---|---|
| 1 | testclient | PASS | 2026-05-26 | 10/10 reconnects clean |
| 2 | xclock | PENDING | — | Not yet run |
| 3 | xterm | PENDING | — | Not yet run |
| 4 | mousepad | PENDING | — | Not yet run |
| 5 | GIMP baseline | PENDING | — | Capture before Stage 6 |
| 6 | gimp | PENDING | — | Not yet run |

## Known Issues
None at this stage.

## Bugs Fixed This Session
1. **testclient: InternAtom only_if_exists=true returns atom 0 on fresh display** — atoms WM_PROTOCOLS and WM_DELETE_WINDOW don't exist until first created; using `only_if_exists=false` (create if absent) fixes the BadAtom error that crashed testclient immediately.
2. **testclient: WM_NAME lost after synthesis** — synthesis recreates the window but not its properties (ChangeProperty not tracked). testclient now re-sets WM_NAME in its Expose handler; synthesis injects Expose so xdotool finds it.
3. **Xvfb needs -ac flag** — harness now passes `-ac` (no auth checking) to Xvfb so proxxxy-client can connect without a cookie.

## Next Action
Run: `tests/run_stage.sh --stage 2 --reconnects 10`
Inspect screenshots in results dir.
Fix any failures, then advance to Stage 3.

## Session History
**Session 1 (2026-05-26):** Built full autonomous harness infrastructure (harness, unit tests, state file, GDB MCP config). Fixed testclient crashes (InternAtom bug, WM_NAME loss after synthesis). Stage 1 now passes 10/10 reconnects.

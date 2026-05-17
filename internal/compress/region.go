package compress

import "sync"

// Rect is a 2D rectangle.
type Rect struct{ X, Y, W, H int }

func (r Rect) intersects(s Rect) bool {
	return r.X < s.X+s.W && r.X+r.W > s.X &&
		r.Y < s.Y+s.H && r.Y+r.H > s.Y
}

func (r Rect) contains(s Rect) bool {
	return s.X >= r.X && s.Y >= r.Y &&
		s.X+s.W <= r.X+r.W && s.Y+s.H <= r.Y+r.H
}

type pendingCmd struct {
	raw  []byte
	rect Rect
}

// RegionTracker accumulates draw commands per window and coalesces superseded ones.
type RegionTracker struct {
	mu      sync.Mutex
	windows map[uint32][]pendingCmd
}

func NewRegionTracker() *RegionTracker {
	return &RegionTracker{windows: make(map[uint32][]pendingCmd)}
}

// Add records a draw command for the given window.
// It extracts a bounding rect from the command and removes any earlier
// commands fully covered by this one.
func (rt *RegionTracker) Add(windowID uint32, cmd []byte) {
	r := extractRect(cmd)
	rt.mu.Lock()
	defer rt.mu.Unlock()

	pending := rt.windows[windowID]
	if r.W > 0 && r.H > 0 {
		filtered := pending[:0]
		for _, p := range pending {
			if !r.contains(p.rect) {
				filtered = append(filtered, p)
			}
		}
		pending = filtered
	}
	pending = append(pending, pendingCmd{raw: cmd, rect: r})
	rt.windows[windowID] = pending
}

// Ack marks a region as acknowledged by the client, removing commands
// whose rects lie entirely within the acked region.
func (rt *RegionTracker) Ack(windowID uint32, x, y, w, h int) {
	acked := Rect{X: x, Y: y, W: w, H: h}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	pending := rt.windows[windowID]
	filtered := pending[:0]
	for _, p := range pending {
		if !acked.contains(p.rect) {
			filtered = append(filtered, p)
		}
	}
	rt.windows[windowID] = filtered
}

// Flush returns and clears all pending commands for the given window.
func (rt *RegionTracker) Flush(windowID uint32) [][]byte {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	pending := rt.windows[windowID]
	rt.windows[windowID] = nil
	out := make([][]byte, len(pending))
	for i, p := range pending {
		out[i] = p.raw
	}
	return out
}

// extractRect attempts to extract a bounding rectangle from an X11 draw command.
// Returns a zero Rect if the command does not carry explicit coordinates.
func extractRect(cmd []byte) Rect {
	if len(cmd) < 16 {
		return Rect{}
	}
	opcode := cmd[0]
	switch opcode {
	case 61: // ClearArea: [op][exp][len:2][win:4][x:2][y:2][w:2][h:2]
		return Rect{
			X: int(int16(uint16(cmd[8]) | uint16(cmd[9])<<8)),
			Y: int(int16(uint16(cmd[10]) | uint16(cmd[11])<<8)),
			W: int(uint16(cmd[12]) | uint16(cmd[13])<<8),
			H: int(uint16(cmd[14]) | uint16(cmd[15])<<8),
		}
	case 70: // PolyFillRectangle: [op][0][len:2][draw:4][gc:4][x:2][y:2][w:2][h:2]
		if len(cmd) < 20 {
			return Rect{}
		}
		return Rect{
			X: int(int16(uint16(cmd[12]) | uint16(cmd[13])<<8)),
			Y: int(int16(uint16(cmd[14]) | uint16(cmd[15])<<8)),
			W: int(uint16(cmd[16]) | uint16(cmd[17])<<8),
			H: int(uint16(cmd[18]) | uint16(cmd[19])<<8),
		}
	}
	return Rect{}
}

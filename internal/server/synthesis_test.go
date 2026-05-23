package server

import (
	"encoding/binary"
	"testing"

	"james.id.au/proxxxy/internal/x11"
)

func TestSanitizeCreateWindow_NoColormap(t *testing.T) {
	// 36-byte CreateWindow with value-mask=0x0002 (CWBackPixel only, no colormap).
	req := make([]byte, 36)
	req[0] = x11.OpcodeCreateWindow
	req[1] = 24 // depth
	binary.LittleEndian.PutUint16(req[2:], 9)              // length = 9 units
	binary.LittleEndian.PutUint32(req[4:], 0x02200001)     // wid
	binary.LittleEndian.PutUint32(req[8:], 0x000003f9)     // parent
	binary.LittleEndian.PutUint32(req[24:], 0x00000021)    // visual
	binary.LittleEndian.PutUint32(req[28:], 0x0002)        // value-mask: CWBackPixel
	binary.LittleEndian.PutUint32(req[32:], 0)             // BackPixel = 0

	out := sanitizeCreateWindow(req, binary.LittleEndian)
	if &out[0] != &req[0] {
		t.Error("expected same slice when no CWColormap")
	}
}

func TestSanitizeCreateWindow_StripsColormap(t *testing.T) {
	// 52-byte CreateWindow: value-mask 0x281a (CWBackPixel|CWBorderPixel|CWBitGravity|CWEventMask|CWColormap).
	// 5 values × 4 bytes = 20 bytes of value-list → total 52 bytes.
	req := make([]byte, 52)
	req[0] = x11.OpcodeCreateWindow
	req[1] = 24 // depth
	binary.LittleEndian.PutUint16(req[2:], 13)              // length = 13 units = 52 bytes
	binary.LittleEndian.PutUint32(req[4:], 0x02200003)      // wid
	binary.LittleEndian.PutUint32(req[8:], 0x000003f9)      // parent
	binary.LittleEndian.PutUint16(req[16:], 640)            // width
	binary.LittleEndian.PutUint16(req[18:], 480)            // height
	binary.LittleEndian.PutUint32(req[24:], 0x00000301)     // visual (non-default)
	binary.LittleEndian.PutUint32(req[28:], 0x0000281a)     // value-mask
	// value-list: BackPixel, BorderPixel, BitGravity, EventMask, Colormap
	binary.LittleEndian.PutUint32(req[32:], 0)              // BackPixel=0
	binary.LittleEndian.PutUint32(req[36:], 0)              // BorderPixel=0
	binary.LittleEndian.PutUint32(req[40:], 1)              // BitGravity=NorthWest
	binary.LittleEndian.PutUint32(req[44:], 0)              // EventMask=0
	binary.LittleEndian.PutUint32(req[48:], 0x02004200)     // Colormap (external, from dead conn)

	out := sanitizeCreateWindow(req, binary.LittleEndian)

	if len(out) != 48 {
		t.Fatalf("expected len=48 (colormap stripped), got %d", len(out))
	}
	// Length field updated.
	if gotLen := binary.LittleEndian.Uint16(out[2:]); gotLen != 12 {
		t.Errorf("length field: got %d, want 12", gotLen)
	}
	// depth and visual are preserved (we keep originals so depth-32 ARGB
	// windows continue to work on the synthesis display).
	if out[1] != 24 {
		t.Errorf("depth: got %d, want 24 (preserved)", out[1])
	}
	if gotVis := binary.LittleEndian.Uint32(out[24:]); gotVis != 0x00000301 {
		t.Errorf("visual: got 0x%x, want 0x301 (preserved)", gotVis)
	}
	// CWColormap bit cleared from value-mask.
	if gotMask := binary.LittleEndian.Uint32(out[28:]); gotMask&(1<<13) != 0 {
		t.Errorf("CWColormap still set in value-mask: 0x%x", gotMask)
	}
	// Remaining values preserved (BackPixel at off 32, BitGravity at off 40).
	if binary.LittleEndian.Uint32(out[40:]) != 1 {
		t.Errorf("BitGravity value corrupted: got %d, want 1", binary.LittleEndian.Uint32(out[40:]))
	}
}

func TestSanitizeGCDrawable_ValidWindow(t *testing.T) {
	req := make([]byte, 20)
	req[0] = x11.OpcodeCreateGC
	binary.LittleEndian.PutUint32(req[4:], 0x02200001) // gc id
	binary.LittleEndian.PutUint32(req[8:], 0x000003f9) // drawable = root window (not in our map)

	windows := map[uint32]x11.Window{0x000003f9: {ID: 0x000003f9}}
	pixmaps := map[uint32]x11.Pixmap{}

	out := sanitizeGCDrawable(req, 0x000003f9, 0, windows, pixmaps, binary.LittleEndian)
	if &out[0] != &req[0] {
		t.Error("expected same slice when drawable is valid window")
	}
}

func TestSanitizeGCDrawable_ValidPixmap(t *testing.T) {
	req := make([]byte, 20)
	req[0] = x11.OpcodeCreateGC
	binary.LittleEndian.PutUint32(req[4:], 0x0220000f)  // gc id
	binary.LittleEndian.PutUint32(req[8:], 0x02200007)  // drawable = existing pixmap

	windows := map[uint32]x11.Window{}
	pixmaps := map[uint32]x11.Pixmap{0x02200007: {ID: 0x02200007}}

	out := sanitizeGCDrawable(req, 0x02200007, 0, windows, pixmaps, binary.LittleEndian)
	if &out[0] != &req[0] {
		t.Error("expected same slice when drawable is valid pixmap")
	}
}

func TestSanitizeGCDrawable_MissingDrawable(t *testing.T) {
	req := make([]byte, 20)
	req[0] = x11.OpcodeCreateGC
	binary.LittleEndian.PutUint32(req[4:], 0x0220000f)  // gc id
	binary.LittleEndian.PutUint32(req[8:], 0x0220000d)  // drawable = freed pixmap

	windows := map[uint32]x11.Window{0x02200003: {ID: 0x02200003}}
	pixmaps := map[uint32]x11.Pixmap{}

	out := sanitizeGCDrawable(req, 0x0220000d, 0, windows, pixmaps, binary.LittleEndian)

	if &out[0] == &req[0] {
		t.Error("expected new slice (drawable was substituted)")
	}
	gotDrawable := binary.LittleEndian.Uint32(out[8:])
	if gotDrawable != 0x02200003 {
		t.Errorf("substituted drawable: got 0x%08x, want 0x02200003", gotDrawable)
	}
}

func TestSanitizeGCDrawable_StripsGCTile(t *testing.T) {
	// CreateGC with value-mask = GCForeground|GCBackground|GCTile (bits 4,5,10).
	// value-list: Foreground=0xff, Background=0x00, Tile=0x02200012 (freed pixmap).
	// 3 values × 4 bytes = 12 bytes; total = 16 + 12 = 28 bytes.
	req := make([]byte, 28)
	req[0] = x11.OpcodeCreateGC
	binary.LittleEndian.PutUint16(req[2:], 7)           // length = 28/4 = 7 units
	binary.LittleEndian.PutUint32(req[4:], 0x0220000f)  // gc id
	binary.LittleEndian.PutUint32(req[8:], 0x0220000d)  // drawable = freed pixmap
	binary.LittleEndian.PutUint32(req[12:], (1<<4)|(1<<5)|(1<<10)) // value-mask
	binary.LittleEndian.PutUint32(req[16:], 0xff)       // GCForeground
	binary.LittleEndian.PutUint32(req[20:], 0x00)       // GCBackground
	binary.LittleEndian.PutUint32(req[24:], 0x02200012) // GCTile (freed)

	windows := map[uint32]x11.Window{0x02200003: {ID: 0x02200003}}
	pixmaps := map[uint32]x11.Pixmap{}

	out := sanitizeGCDrawable(req, 0x0220000d, 0, windows, pixmaps, binary.LittleEndian)

	if len(out) != 24 {
		t.Fatalf("expected len=24 (tile stripped), got %d", len(out))
	}
	// GCTile bit cleared.
	gotMask := binary.LittleEndian.Uint32(out[12:])
	if gotMask&(1<<10) != 0 {
		t.Errorf("GCTile still set in value-mask: 0x%x", gotMask)
	}
	// GCForeground and GCBackground preserved.
	if binary.LittleEndian.Uint32(out[16:]) != 0xff {
		t.Errorf("GCForeground corrupted")
	}
	if binary.LittleEndian.Uint32(out[20:]) != 0x00 {
		t.Errorf("GCBackground corrupted")
	}
	// Length field updated.
	if gotLen := binary.LittleEndian.Uint16(out[2:]); gotLen != 6 {
		t.Errorf("length field: got %d, want 6", gotLen)
	}
}

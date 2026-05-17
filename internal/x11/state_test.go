package x11_test

import (
	"encoding/binary"
	"testing"

	"james.id.au/proxxxy/internal/x11"
)

func TestAppConnTrackWindow(t *testing.T) {
	conn := x11.NewAppConn(1, binary.LittleEndian)

	// Simulate CreateWindow bytes: opcode=1, depth=24, length=8 (32 bytes)
	// Fields: [wid:4][parent:4][x:2][y:2][w:2][h:2][border:2][class:2][visual:4][value-mask:4]
	req := make([]byte, 32)
	req[0] = x11.OpcodeCreateWindow
	req[1] = 24 // depth
	binary.LittleEndian.PutUint16(req[2:], 8)
	binary.LittleEndian.PutUint32(req[4:], 0x100001) // wid
	binary.LittleEndian.PutUint32(req[8:], 0x000001) // parent (root)
	binary.LittleEndian.PutUint16(req[12:], 0)       // x
	binary.LittleEndian.PutUint16(req[14:], 0)       // y
	binary.LittleEndian.PutUint16(req[16:], 300)     // width
	binary.LittleEndian.PutUint16(req[18:], 300)     // height
	binary.LittleEndian.PutUint16(req[20:], 1)       // border-width
	binary.LittleEndian.PutUint16(req[22:], 1)       // class=InputOutput

	conn.ProcessRequest(req)

	w := conn.Window(0x100001)
	if w == nil {
		t.Fatal("window not tracked")
	}
	if w.Width != 300 || w.Height != 300 {
		t.Fatalf("geometry: got %dx%d want 300x300", w.Width, w.Height)
	}
}

func TestAppConnTrackGC(t *testing.T) {
	conn := x11.NewAppConn(1, binary.LittleEndian)

	// CreateGC: opcode=55, length=4+(values), drawable=0x100001, gc=0x200001, mask=GCForeground(4)
	req := make([]byte, 16)
	req[0] = x11.OpcodeCreateGC
	binary.LittleEndian.PutUint16(req[2:], 4)        // length=16 bytes
	binary.LittleEndian.PutUint32(req[4:], 0x200001) // gc id
	binary.LittleEndian.PutUint32(req[8:], 0x100001) // drawable
	binary.LittleEndian.PutUint32(req[12:], 1<<2)    // GCForeground mask

	conn.ProcessRequest(req)

	gc := conn.GC(0x200001)
	if gc == nil {
		t.Fatal("GC not tracked")
	}
	if gc.Drawable != 0x100001 {
		t.Fatalf("drawable: got %x want %x", gc.Drawable, 0x100001)
	}
}

func TestAppConnMapWindow(t *testing.T) {
	conn := x11.NewAppConn(1, binary.LittleEndian)

	// First create the window.
	req := make([]byte, 32)
	req[0] = x11.OpcodeCreateWindow
	req[1] = 24
	binary.LittleEndian.PutUint16(req[2:], 8)
	binary.LittleEndian.PutUint32(req[4:], 0x100002)
	binary.LittleEndian.PutUint32(req[8:], 0x000001)
	binary.LittleEndian.PutUint16(req[16:], 200)
	binary.LittleEndian.PutUint16(req[18:], 200)
	conn.ProcessRequest(req)

	// MapWindow.
	mapReq := make([]byte, 8)
	mapReq[0] = x11.OpcodeMapWindow
	binary.LittleEndian.PutUint16(mapReq[2:], 2)
	binary.LittleEndian.PutUint32(mapReq[4:], 0x100002)
	conn.ProcessRequest(mapReq)

	w := conn.Window(0x100002)
	if w == nil || !w.Mapped {
		t.Fatal("window should be mapped")
	}
}

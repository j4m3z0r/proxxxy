package x11_test

import (
	"encoding/binary"
	"testing"

	"james.id.au/proxxxy/internal/x11"
)

func makeWindow(wid, parent uint32) []byte {
	req := make([]byte, 32)
	req[0] = x11.OpcodeCreateWindow
	req[1] = 24
	binary.LittleEndian.PutUint16(req[2:], 8)
	binary.LittleEndian.PutUint32(req[4:], wid)
	binary.LittleEndian.PutUint32(req[8:], parent)
	binary.LittleEndian.PutUint16(req[16:], 300)
	binary.LittleEndian.PutUint16(req[18:], 300)
	return req
}

func makeConfigureWindow(wid, w, h uint32) []byte {
	// [opcode:1][pad:1][len:2][window:4][value-mask:2][pad:2][w:4][h:4]
	req := make([]byte, 20)
	req[0] = x11.OpcodeConfigureWindow
	binary.LittleEndian.PutUint16(req[2:], 5) // 20 bytes / 4
	binary.LittleEndian.PutUint32(req[4:], wid)
	binary.LittleEndian.PutUint16(req[8:], (1<<2)|(1<<3)) // CWWidth | CWHeight
	binary.LittleEndian.PutUint32(req[12:], w)
	binary.LittleEndian.PutUint32(req[16:], h)
	return req
}

func TestDrainPendingConfigures_Count(t *testing.T) {
	conn := x11.NewAppConn(1, binary.LittleEndian)
	conn.ProcessRequest(makeWindow(0x100001, 0x000001))
	for i := 0; i < 3; i++ {
		conn.ProcessRequest(makeConfigureWindow(0x100001, 300, 300))
	}

	counts := conn.DrainPendingConfigures()
	if n := counts[0x100001]; n != 3 {
		t.Errorf("PendingConfigures: got %d, want 3", n)
	}
}

func TestDrainPendingConfigures_DrainResetsCount(t *testing.T) {
	conn := x11.NewAppConn(1, binary.LittleEndian)
	conn.ProcessRequest(makeWindow(0x100001, 0x000001))
	conn.ProcessRequest(makeConfigureWindow(0x100001, 300, 300))
	_ = conn.DrainPendingConfigures()

	counts := conn.DrainPendingConfigures()
	if n := counts[0x100001]; n != 0 {
		t.Errorf("after drain: got %d, want 0", n)
	}
}

func TestDrainPendingConfigures_ResetBeforeDrain(t *testing.T) {
	conn := x11.NewAppConn(1, binary.LittleEndian)
	conn.ProcessRequest(makeWindow(0x100001, 0x000001))
	conn.ProcessRequest(makeConfigureWindow(0x100001, 300, 300))
	conn.ProcessRequest(makeConfigureWindow(0x100001, 400, 400))
	conn.ResetPendingConfigures()
	conn.ProcessRequest(makeConfigureWindow(0x100001, 500, 500))

	counts := conn.DrainPendingConfigures()
	if n := counts[0x100001]; n != 1 {
		t.Errorf("post-reset drain: got %d, want 1", n)
	}
}

func TestDrainPendingConfigures_MultipleWindows(t *testing.T) {
	conn := x11.NewAppConn(1, binary.LittleEndian)
	conn.ProcessRequest(makeWindow(0x100001, 0x000001))
	conn.ProcessRequest(makeWindow(0x100002, 0x000001))
	conn.ProcessRequest(makeConfigureWindow(0x100001, 300, 300))
	conn.ProcessRequest(makeConfigureWindow(0x100001, 300, 300))
	conn.ProcessRequest(makeConfigureWindow(0x100002, 200, 200))

	counts := conn.DrainPendingConfigures()
	if n := counts[0x100001]; n != 2 {
		t.Errorf("window 1: got %d, want 2", n)
	}
	if n := counts[0x100002]; n != 1 {
		t.Errorf("window 2: got %d, want 1", n)
	}
}

package client

import (
	"encoding/binary"
	"testing"

	"james.id.au/proxxxy/internal/x11"
)

func le32(b []byte, off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }
func le16(b []byte, off int, v uint16) { binary.LittleEndian.PutUint16(b[off:], v) }
func get32(b []byte, off int) uint32   { return binary.LittleEndian.Uint32(b[off:]) }

// testRemap returns a standard idRemap: old=0x02000000/0x001FFFFF → new=0x03000000.
func testRemap() idRemap {
	return idRemap{
		oldBase: 0x02000000,
		oldMask: 0x001FFFFF,
		newBase: 0x03000000,
		order:   binary.LittleEndian,
	}
}

func TestApplyIDRemap_CreateWindow(t *testing.T) {
	// CreateWindow: [opcode:1][depth:1][len:2][wid:4][parent:4][x:2]...
	cmd := make([]byte, 32)
	cmd[0] = x11.OpcodeCreateWindow
	le16(cmd, 2, 8) // length = 8 units = 32 bytes
	le32(cmd, 4, 0x02000001)
	le32(cmd, 8, 0x02000002)

	got := applyIDRemap(cmd, testRemap())

	if wid := get32(got, 4); wid != 0x03000001 {
		t.Errorf("wid: got 0x%08x, want 0x03000001", wid)
	}
	if par := get32(got, 8); par != 0x03000002 {
		t.Errorf("parent: got 0x%08x, want 0x03000002", par)
	}
}

func TestApplyIDRemap_OutOfRange(t *testing.T) {
	// Root window (0x00000038) must NOT be remapped.
	cmd := make([]byte, 32)
	cmd[0] = x11.OpcodeCreateWindow
	le16(cmd, 2, 8)
	le32(cmd, 4, 0x02000001) // in range → remap
	le32(cmd, 8, 0x00000038) // root, NOT in range → unchanged

	got := applyIDRemap(cmd, testRemap())

	if wid := get32(got, 4); wid != 0x03000001 {
		t.Errorf("wid: got 0x%08x, want 0x03000001", wid)
	}
	if par := get32(got, 8); par != 0x00000038 {
		t.Errorf("parent: got 0x%08x, want 0x00000038 (unchanged)", par)
	}
}

func TestApplyIDRemap_ShmPutImage(t *testing.T) {
	// MIT-SHM ShmPutImage (minor=3): drawable[4] gc[8] shmseg[32]
	cmd := make([]byte, 40)
	cmd[0] = x11.OpcodeMITSHM
	cmd[1] = 3 // ShmPutImage minor opcode
	le16(cmd, 2, 10)
	le32(cmd, 4, 0x02000001)  // drawable
	le32(cmd, 8, 0x02000002)  // gc
	le32(cmd, 32, 0x02000003) // shmseg

	got := applyIDRemap(cmd, testRemap())

	if v := get32(got, 4); v != 0x03000001 {
		t.Errorf("drawable: got 0x%08x, want 0x03000001", v)
	}
	if v := get32(got, 8); v != 0x03000002 {
		t.Errorf("gc: got 0x%08x, want 0x03000002", v)
	}
	if v := get32(got, 32); v != 0x03000003 {
		t.Errorf("shmseg: got 0x%08x, want 0x03000003", v)
	}
}

func TestApplyIDRemap_ShmCreatePixmap(t *testing.T) {
	// MIT-SHM ShmCreatePixmap (minor=5): pixmap[4] drawable[8] shmseg[20]
	cmd := make([]byte, 28)
	cmd[0] = x11.OpcodeMITSHM
	cmd[1] = x11.SHMCreatePixmap
	le16(cmd, 2, 7)
	le32(cmd, 4, 0x02000001)  // pixmap ID
	le32(cmd, 8, 0x02000002)  // drawable
	le32(cmd, 20, 0x02000003) // shmseg

	got := applyIDRemap(cmd, testRemap())

	if v := get32(got, 4); v != 0x03000001 {
		t.Errorf("pixmap: got 0x%08x, want 0x03000001", v)
	}
	if v := get32(got, 8); v != 0x03000002 {
		t.Errorf("drawable: got 0x%08x, want 0x03000002", v)
	}
	if v := get32(got, 20); v != 0x03000003 {
		t.Errorf("shmseg: got 0x%08x, want 0x03000003", v)
	}
}

func TestApplyIDRemap_RenderCreatePicture(t *testing.T) {
	// RENDER CreatePicture (minor=4): pid[4] drawable[8]
	cmd := make([]byte, 24)
	cmd[0] = x11.OpcodeRender
	cmd[1] = x11.RenderCreatePicture
	le16(cmd, 2, 6)
	le32(cmd, 4, 0x02000001) // pid
	le32(cmd, 8, 0x02000002) // drawable

	got := applyIDRemap(cmd, testRemap())

	if v := get32(got, 4); v != 0x03000001 {
		t.Errorf("pid: got 0x%08x, want 0x03000001", v)
	}
	if v := get32(got, 8); v != 0x03000002 {
		t.Errorf("drawable: got 0x%08x, want 0x03000002", v)
	}
}

func TestApplyIDRemap_BigRequest(t *testing.T) {
	// BigRequest: standard len field == 0, real length at [4:8], body shifted by s=4.
	// Test CreatePixmap: pixmap[4+4=8] drawable[8+4=12].
	cmd := make([]byte, 24)
	cmd[0] = x11.OpcodeCreatePixmap
	le16(cmd, 2, 0) // BigRequest sentinel
	le32(cmd, 4, 6) // real length (24 bytes / 4)
	le32(cmd, 8, 0x02000001)  // pixmap ID
	le32(cmd, 12, 0x02000002) // drawable

	got := applyIDRemap(cmd, testRemap())

	if v := get32(got, 8); v != 0x03000001 {
		t.Errorf("pixmap: got 0x%08x, want 0x03000001", v)
	}
	if v := get32(got, 12); v != 0x03000002 {
		t.Errorf("drawable: got 0x%08x, want 0x03000002", v)
	}
}

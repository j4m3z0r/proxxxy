package compress_test

import (
	"encoding/binary"
	"testing"

	"james.id.au/proxxxy/internal/compress"
)

func TestRegionCoalesceClearThenFill(t *testing.T) {
	// ClearArea followed by PolyFillRectangle covering the same area should
	// coalesce to just the PolyFillRectangle.
	tracker := compress.NewRegionTracker()

	// ClearArea: [61][0][len:2][window:4][x:2][y:2][w:2][h:2][exposures:1]
	clear := []byte{
		61, 0, 4, 0,
		0x01, 0x00, 0x10, 0x00, // window
		50, 0, 50, 0,           // x=50, y=50
		200, 0, 200, 0,         // w=200, h=200
		0, 0, 0, 0,
	}
	// PolyFillRectangle: [70][0][len:2][drawable:4][gc:4][x:2][y:2][w:2][h:2]
	fill := []byte{
		70, 0, 4, 0,
		0x01, 0x00, 0x10, 0x00, // drawable (same as window)
		0x01, 0x00, 0x20, 0x00, // gc
		50, 0, 50, 0,
		200, 0, 200, 0,
	}

	tracker.Add(0x00100001, clear, binary.LittleEndian)
	tracker.Add(0x00100001, fill, binary.LittleEndian)

	coalesced := tracker.Flush(0x00100001)
	if len(coalesced) != 1 {
		t.Fatalf("expected 1 command after coalescing, got %d", len(coalesced))
	}
	if coalesced[0][0] != 70 {
		t.Fatalf("expected PolyFillRectangle (70) after coalescing, got %d", coalesced[0][0])
	}
}

func TestRegionAck(t *testing.T) {
	tracker := compress.NewRegionTracker()
	fill := []byte{70, 0, 4, 0, 0x01, 0x00, 0x10, 0x00, 0x01, 0x00, 0x20, 0x00,
		50, 0, 50, 0, 200, 0, 200, 0}
	tracker.Add(0x00100001, fill, binary.LittleEndian)

	// Ack the region — pending commands should be cleared.
	tracker.Ack(0x00100001, 50, 50, 200, 200)
	remaining := tracker.Flush(0x00100001)
	if len(remaining) != 0 {
		t.Fatalf("expected 0 commands after ack, got %d", len(remaining))
	}
}

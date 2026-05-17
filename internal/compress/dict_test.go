package compress_test

import (
	"testing"

	"james.id.au/proxxxy/internal/compress"
)

func TestDictFirstOccurrenceReturnsDefine(t *testing.T) {
	d := compress.NewDict(1024 * 1024) // 1 MB
	seq := []byte{0x01, 0x02, 0x03, 0x04}
	action, id, data := d.Classify(seq)
	if action != compress.ActionDefine {
		t.Fatalf("first occurrence: want ActionDefine got %v", action)
	}
	if len(data) == 0 {
		t.Fatal("define action must carry the data")
	}
	_ = id
}

func TestDictSecondOccurrenceReturnsRef(t *testing.T) {
	d := compress.NewDict(1024 * 1024)
	seq := []byte{0xAA, 0xBB, 0xCC}
	action, id1, _ := d.Classify(seq)
	if action != compress.ActionDefine {
		t.Fatalf("first: want ActionDefine got %v", action)
	}
	action, id2, _ := d.Classify(seq)
	if action != compress.ActionRef {
		t.Fatalf("second: want ActionRef got %v", action)
	}
	if id1 != id2 {
		t.Fatalf("id mismatch: %d != %d", id1, id2)
	}
}

func TestDictEvictsWhenFull(t *testing.T) {
	// 8-byte capacity: first entry fills it, second forces eviction.
	d := compress.NewDict(8)
	a := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	b := []byte{9, 10, 11, 12, 13, 14, 15, 16}
	action, idA, _ := d.Classify(a)
	if action != compress.ActionDefine {
		t.Fatal("first: want ActionDefine")
	}
	action, _, _ = d.Classify(b)
	if action != compress.ActionDefine {
		t.Fatal("second: want ActionDefine (forces eviction of a)")
	}
	expiredIDs := d.DrainExpired()
	if len(expiredIDs) == 0 {
		t.Fatal("expected eviction of entry A")
	}
	if expiredIDs[0] != idA {
		t.Fatalf("evicted id: got %d want %d", expiredIDs[0], idA)
	}
}

func TestDictNewEntryAfterEvictionIsDefine(t *testing.T) {
	d := compress.NewDict(8)
	a := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	b := []byte{9, 10, 11, 12, 13, 14, 15, 16}
	d.Classify(a)
	d.Classify(b) // evicts a
	d.DrainExpired()
	// Now a is gone from clientDict; re-adding it should return ActionDefine.
	action, _, _ := d.Classify(a)
	if action != compress.ActionDefine {
		t.Fatalf("re-entry after eviction: want ActionDefine got %v", action)
	}
}

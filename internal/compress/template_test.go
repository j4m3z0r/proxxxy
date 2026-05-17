package compress_test

import (
	"testing"

	"james.id.au/proxxxy/internal/compress"
)

func TestTemplateExtractAndApply(t *testing.T) {
	// Two PolyFillRectangle requests that differ only in coordinate bytes.
	// Format: [opcode:1][pad:1][len:2][drawable:4][gc:4][x:2][y:2][w:2][h:2]
	make16 := func(x, y, w, h int16) []byte {
		b := []byte{
			0x46, 0x00, 0x04, 0x00, // opcode=70, pad, length=4 units
			0x01, 0x00, 0x10, 0x00, // drawable
			0x01, 0x00, 0x20, 0x00, // gc
			byte(x), byte(x >> 8), byte(y), byte(y >> 8),
			byte(w), byte(w >> 8), byte(h), byte(h >> 8),
		}
		return b
	}

	seq1 := make16(10, 20, 100, 100)
	seq2 := make16(15, 20, 100, 100) // only x changed

	tr := compress.NewTemplateRegistry()
	tmpl, params, isNew := tr.Observe(seq1, seq2)

	if !isNew {
		t.Fatal("first pair should produce a new template")
	}
	if tmpl == nil {
		t.Fatal("expected a template")
	}

	// Apply the template with params to reconstruct seq2.
	got, err := tmpl.Apply(params)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(seq2) {
		t.Fatalf("apply: got %v want %v", got, seq2)
	}
}

// TestTemplateNoReuseOnNonSlotDiff verifies that a template is NOT reused when
// the non-slot bytes of the new command differ from the template base. Previously
// this caused wrong bytes to be reconstructed (e.g. wrong GC id in ChangeGC).
func TestTemplateNoReuseOnNonSlotDiff(t *testing.T) {
	// ChangeGC-like commands: [opcode:1][pad:1][len:2][gc_id:4][mask:4][foreground:4]
	makeCmd := func(gcID uint32, fg uint32) []byte {
		b := make([]byte, 16)
		b[0] = 56 // opcode
		b[1] = 0
		b[2], b[3] = 4, 0 // length
		b[4] = byte(gcID); b[5] = byte(gcID >> 8); b[6] = byte(gcID >> 16); b[7] = byte(gcID >> 24)
		b[8], b[9], b[10], b[11] = 4, 0, 0, 0 // mask: foreground bit
		b[12] = byte(fg); b[13] = byte(fg >> 8); b[14] = byte(fg >> 16); b[15] = byte(fg >> 24)
		return b
	}

	gc1Black := makeCmd(1, 0x000000)
	gc1White := makeCmd(1, 0xFFFFFF)
	gc2Black := makeCmd(2, 0x000000)
	gc2White := makeCmd(2, 0xFFFFFF) // non-slot gc_id differs from template base (gc1)

	tr := compress.NewTemplateRegistry()
	tr.Observe(gc1Black, gc1White) // creates template T: base=gc1Black, slots=[foreground]
	tr.Observe(gc1White, gc2Black) // different gc_id — creates new template T2
	// prev is now gc2Black; gc2White differs from gc2Black only at foreground (same slots as T)
	// but T's base has gc1 — must NOT reuse T (would produce gc1White instead of gc2White)
	tmpl, params, isNew := tr.Observe(gc2Black, gc2White)

	if tmpl == nil {
		t.Fatal("expected a template (new or existing) for gc2White")
	}
	got, err := tmpl.Apply(params)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(gc2White) {
		t.Fatalf("wrong reconstruction: got gc_id=%d fg=%x, want gc_id=2 fg=ffffff",
			uint32(got[4])|uint32(got[5])<<8|uint32(got[6])<<16|uint32(got[7])<<24,
			uint32(got[12])|uint32(got[13])<<8|uint32(got[14])<<16|uint32(got[15])<<24)
	}
	// If isNew=false it reused an existing template; if isNew=true it made a new one.
	// Either is acceptable as long as reconstruction is correct.
	_ = isNew
}

func TestTemplateRefOnRepeat(t *testing.T) {
	make16 := func(x int16) []byte {
		return []byte{
			0x46, 0x00, 0x04, 0x00,
			0x01, 0x00, 0x10, 0x00,
			0x01, 0x00, 0x20, 0x00,
			byte(x), byte(x >> 8), 0x00, 0x00,
			0x64, 0x00, 0x64, 0x00,
		}
	}
	seq1 := make16(10)
	seq2 := make16(15)
	seq3 := make16(20)

	tr := compress.NewTemplateRegistry()
	tr.Observe(seq1, seq2) // creates template
	tmpl, params, isNew := tr.Observe(seq2, seq3)

	if isNew {
		t.Fatal("second pair should reuse existing template")
	}
	got, err := tmpl.Apply(params)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(seq3) {
		t.Fatalf("apply: got %v want %v", got, seq3)
	}
}

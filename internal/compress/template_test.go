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

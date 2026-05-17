package compress

import (
	"fmt"
	"sync"
)

// ParamSlot describes one differing byte range between two sequences.
type ParamSlot struct {
	Offset int
	Length int
}

// Template is a sequence with differing byte ranges replaced by slots.
type Template struct {
	ID    uint64
	base  []byte      // the first sequence, as the template body
	slots []ParamSlot // where parameters go
}

// Apply reconstructs a sequence from the template by substituting params.
func (t *Template) Apply(params [][]byte) ([]byte, error) {
	if len(params) != len(t.slots) {
		return nil, fmt.Errorf("template: want %d params got %d", len(t.slots), len(params))
	}
	out := make([]byte, len(t.base))
	copy(out, t.base)
	for i, s := range t.slots {
		if len(params[i]) != s.Length {
			return nil, fmt.Errorf("template: param %d: want %d bytes got %d", i, s.Length, len(params[i]))
		}
		copy(out[s.Offset:], params[i])
	}
	return out, nil
}

// Slots returns the parameter slots (for building TEMPLATE_DEFINE messages).
func (t *Template) Slots() []ParamSlot { return t.slots }

// TemplateRegistry identifies structural similarities between command sequences
// and manages parametric templates.
type TemplateRegistry struct {
	mu       sync.Mutex
	nextID   uint64
	byOpcode map[byte][]*Template
}

func NewTemplateRegistry() *TemplateRegistry {
	return &TemplateRegistry{byOpcode: make(map[byte][]*Template)}
}

// Observe compares seq2 against seq1 (the previous sequence with the same opcode).
// If they are structurally compatible (same length, same opcode, differ only in
// numeric slots), returns the template, the parameter values for seq2, and whether
// a new template was created.
func (tr *TemplateRegistry) Observe(seq1, seq2 []byte) (*Template, [][]byte, bool) {
	if len(seq1) != len(seq2) || len(seq1) == 0 || seq1[0] != seq2[0] {
		return nil, nil, false
	}

	slots, _ := diffSlots(seq1, seq2)
	if len(slots) == 0 {
		return nil, nil, false // identical sequences, use dict instead
	}

	tr.mu.Lock()
	defer tr.mu.Unlock()

	opcode := seq1[0]
	for _, tmpl := range tr.byOpcode[opcode] {
		if slotsMatch(tmpl.slots, slots) {
			p := extractParams(seq2, slots)
			return tmpl, p, false
		}
	}

	// New template.
	tr.nextID++
	base := make([]byte, len(seq1))
	copy(base, seq1)
	tmpl := &Template{ID: tr.nextID, base: base, slots: slots}
	tr.byOpcode[opcode] = append(tr.byOpcode[opcode], tmpl)
	p := extractParams(seq2, slots)
	return tmpl, p, true
}

// diffSlots finds contiguous byte ranges where a and b differ.
func diffSlots(a, b []byte) ([]ParamSlot, [][]byte) {
	var slots []ParamSlot
	var params [][]byte
	i := 0
	for i < len(a) {
		if a[i] == b[i] {
			i++
			continue
		}
		start := i
		for i < len(a) && a[i] != b[i] {
			i++
		}
		slots = append(slots, ParamSlot{Offset: start, Length: i - start})
		params = append(params, b[start:i])
	}
	return slots, params
}

func extractParams(seq []byte, slots []ParamSlot) [][]byte {
	params := make([][]byte, len(slots))
	for i, s := range slots {
		params[i] = seq[s.Offset : s.Offset+s.Length]
	}
	return params
}

func slotsMatch(a, b []ParamSlot) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

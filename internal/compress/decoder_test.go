package compress

import (
	"bytes"
	"encoding/binary"
	"testing"

	"james.id.au/proxxxy/internal/wire"
)

func TestRoundTrip(t *testing.T) {
	const connID uint32 = 7
	enc := NewEncoder(connID, 4*1024*1024)
	dec := NewDecoder()

	// decodeAll processes a slice of wire messages and returns the last
	// non-nil data payload, asserting connID is correct on every message.
	decodeAll := func(msgs []wire.Msg) []byte {
		var result []byte
		for _, m := range msgs {
			gotConnID, data, err := dec.Decode(m)
			if err != nil {
				t.Fatalf("decode 0x%02x: %v", m.Type, err)
			}
			if gotConnID != connID {
				t.Fatalf("connID: got %d want %d", gotConnID, connID)
			}
			if data != nil {
				result = data
			}
		}
		return result
	}

	encode := func(cmd []byte) []byte {
		msgs := enc.Encode(0, cmd, binary.LittleEndian)
		msgs = append(msgs, enc.DrainExpiredDicts()...)
		return decodeAll(msgs)
	}

	// Dict path: first occurrence → MsgDictDefine, second → MsgDictRef.
	cmd1 := []byte{70, 0, 4, 0, 1, 0, 0x10, 0, 1, 0, 0x20, 0, 50, 0, 50, 0}
	if got := encode(cmd1); !bytes.Equal(got, cmd1) {
		t.Fatalf("dict define: got %x want %x", got, cmd1)
	}
	if got := encode(cmd1); !bytes.Equal(got, cmd1) {
		t.Fatalf("dict ref: got %x want %x", got, cmd1)
	}

	// Template path: two commands with same opcode and length that differ
	// only in their last 4 bytes. First triggers DictDefine; second triggers
	// TemplateDefine + TemplateApply.
	cmd2a := []byte{55, 0, 4, 0, 0xAA, 0xBB, 0xCC, 0xDD, 0x01, 0x02, 0x03, 0x04, 0xE1, 0xE2, 0xE3, 0xE4}
	cmd2b := []byte{55, 0, 4, 0, 0xAA, 0xBB, 0xCC, 0xDD, 0x01, 0x02, 0x03, 0x04, 0xFF, 0xFF, 0xFF, 0xFF}
	encode(cmd2a) // DictDefine for cmd2a; sets prev[55]=cmd2a.
	if got := encode(cmd2b); !bytes.Equal(got, cmd2b) {
		t.Fatalf("template define+apply: got %x want %x", got, cmd2b)
	}
}

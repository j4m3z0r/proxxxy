package compress

import (
	"encoding/binary"
	"fmt"

	"james.id.au/proxxxy/internal/wire"
)

// Decoder is the client-side counterpart to Encoder. One instance is kept
// per X11 connection (keyed by connID). It maintains a dict and template
// registry that mirror the encoder's state.
type Decoder struct {
	dict      map[uint64][]byte
	templates map[uint64]*Template
}

func NewDecoder() *Decoder {
	return &Decoder{
		dict:      make(map[uint64][]byte),
		templates: make(map[uint64]*Template),
	}
}

// Decode reconstructs the original X11 bytes from a compressed wire message.
// Returns connID extracted from the payload, the reconstructed bytes (nil for
// define/expire messages that carry no X11 data), and any parse error.
// MsgX11Data is not handled here; the caller continues to handle it directly.
func (d *Decoder) Decode(msg wire.Msg) (connID uint32, data []byte, err error) {
	if len(msg.Payload) < 4 {
		return 0, nil, fmt.Errorf("decoder: payload too short (%d bytes)", len(msg.Payload))
	}
	connID = binary.LittleEndian.Uint32(msg.Payload[:4])
	rest := msg.Payload[4:]

	switch msg.Type {
	case wire.MsgDictDefine:
		if len(rest) < 8 {
			return 0, nil, fmt.Errorf("decoder: DictDefine too short")
		}
		id := binary.LittleEndian.Uint64(rest[:8])
		entry := make([]byte, len(rest)-8)
		copy(entry, rest[8:])
		d.dict[id] = entry
		return connID, entry, nil

	case wire.MsgDictRef:
		if len(rest) < 8 {
			return 0, nil, fmt.Errorf("decoder: DictRef too short")
		}
		id := binary.LittleEndian.Uint64(rest[:8])
		entry, ok := d.dict[id]
		if !ok {
			return 0, nil, fmt.Errorf("decoder: DictRef unknown id %d", id)
		}
		return connID, entry, nil

	case wire.MsgDictExpire:
		if len(rest) < 8 {
			return 0, nil, fmt.Errorf("decoder: DictExpire too short")
		}
		id := binary.LittleEndian.Uint64(rest[:8])
		delete(d.dict, id)
		return connID, nil, nil

	case wire.MsgTemplateDefine:
		if len(rest) < 10 {
			return 0, nil, fmt.Errorf("decoder: TemplateDefine too short")
		}
		id := binary.LittleEndian.Uint64(rest[:8])
		nslots := int(binary.LittleEndian.Uint16(rest[8:10]))
		if len(rest) < 10+nslots*8 {
			return 0, nil, fmt.Errorf("decoder: TemplateDefine slots truncated")
		}
		slots := make([]ParamSlot, nslots)
		for i := range slots {
			off := 10 + i*8
			slots[i].Offset = int(binary.LittleEndian.Uint32(rest[off:]))
			slots[i].Length = int(binary.LittleEndian.Uint32(rest[off+4:]))
		}
		baseOff := 10 + nslots*8
		base := make([]byte, len(rest)-baseOff)
		copy(base, rest[baseOff:])
		d.templates[id] = &Template{ID: id, base: base, slots: slots}
		return connID, nil, nil

	case wire.MsgTemplateApply:
		if len(rest) < 10 {
			return 0, nil, fmt.Errorf("decoder: TemplateApply too short")
		}
		id := binary.LittleEndian.Uint64(rest[:8])
		nparams := int(binary.LittleEndian.Uint16(rest[8:10]))
		tmpl, ok := d.templates[id]
		if !ok {
			return 0, nil, fmt.Errorf("decoder: TemplateApply unknown template id %d", id)
		}
		params := make([][]byte, nparams)
		off := 10
		for i := range params {
			if len(rest) < off+2 {
				return 0, nil, fmt.Errorf("decoder: TemplateApply params truncated at param %d", i)
			}
			plen := int(binary.LittleEndian.Uint16(rest[off:]))
			off += 2
			if len(rest) < off+plen {
				return 0, nil, fmt.Errorf("decoder: TemplateApply param %d data truncated", i)
			}
			params[i] = rest[off : off+plen]
			off += plen
		}
		result, err := tmpl.Apply(params)
		if err != nil {
			return 0, nil, fmt.Errorf("decoder: TemplateApply: %w", err)
		}
		return connID, result, nil

	default:
		return 0, nil, fmt.Errorf("decoder: unexpected message type 0x%02x", msg.Type)
	}
}

package compress

import (
	"encoding/binary"

	"james.id.au/proxxxy/internal/wire"
)

// Encoder wraps Dict + TemplateRegistry + RegionTracker and applies all three
// compression levels to outgoing X11 command sequences.
type Encoder struct {
	dict      *Dict
	templates *TemplateRegistry
	regions   *RegionTracker
	prev      map[byte][]byte // last command seen per opcode, for template detection
}

func NewEncoder(dictCapacity int) *Encoder {
	return &Encoder{
		dict:      NewDict(dictCapacity),
		templates: NewTemplateRegistry(),
		regions:   NewRegionTracker(),
		prev:      make(map[byte][]byte),
	}
}

// OnClientDisconnect resets the client-side view of the dictionary.
func (e *Encoder) OnClientDisconnect() {
	e.dict.ClientDisconnected()
}

// Encode processes one X11 command and returns a sequence of wire messages to
// send to the proxxxy-client. windowID is the target window (0 if not a draw cmd).
func (e *Encoder) Encode(windowID uint32, cmd []byte) []wire.Msg {
	if len(cmd) == 0 {
		return nil
	}

	// Level 5: region coalescing — queue and flush.
	if windowID != 0 {
		e.regions.Add(windowID, cmd)
		cmds := e.regions.Flush(windowID)
		var out []wire.Msg
		for _, c := range cmds {
			out = append(out, e.encodeOne(c)...)
		}
		return out
	}
	return e.encodeOne(cmd)
}

// DrainExpiredDicts returns DICT_EXPIRE messages for evicted entries.
func (e *Encoder) DrainExpiredDicts() []wire.Msg {
	ids := e.dict.DrainExpired()
	msgs := make([]wire.Msg, len(ids))
	for i, id := range ids {
		p := make([]byte, 8)
		binary.LittleEndian.PutUint64(p, id)
		msgs[i] = wire.Msg{Type: wire.MsgDictExpire, Payload: p}
	}
	return msgs
}

func (e *Encoder) encodeOne(cmd []byte) []wire.Msg {
	opcode := cmd[0]

	// Level 3: template detection.
	if prev, ok := e.prev[opcode]; ok {
		tmpl, params, isNew := e.templates.Observe(prev, cmd)
		if tmpl != nil {
			e.prev[opcode] = cmd
			if isNew {
				return []wire.Msg{
					makeTemplateDefine(tmpl),
					makeTemplateApply(tmpl.ID, params),
				}
			}
			return []wire.Msg{makeTemplateApply(tmpl.ID, params)}
		}
	}
	e.prev[opcode] = cmd

	// Level 2: dictionary.
	action, id, data := e.dict.Classify(cmd)
	switch action {
	case ActionDefine:
		return []wire.Msg{makeDictDefine(id, data)}
	case ActionRef:
		return []wire.Msg{makeDictRef(id)}
	}
	// ActionPassthrough — sequence too large for dict. Caller must wrap in X11_DATA.
	return []wire.Msg{{Type: wire.MsgX11Data, Payload: append([]byte{0, 0, 0, 0}, cmd...)}}
}

func makeDictDefine(id uint64, data []byte) wire.Msg {
	p := make([]byte, 8+len(data))
	binary.LittleEndian.PutUint64(p[:8], id)
	copy(p[8:], data)
	return wire.Msg{Type: wire.MsgDictDefine, Payload: p}
}

func makeDictRef(id uint64) wire.Msg {
	p := make([]byte, 8)
	binary.LittleEndian.PutUint64(p, id)
	return wire.Msg{Type: wire.MsgDictRef, Payload: p}
}

func makeTemplateDefine(tmpl *Template) wire.Msg {
	slots := tmpl.Slots()
	p := make([]byte, 8+2+len(slots)*8+len(tmpl.base))
	binary.LittleEndian.PutUint64(p[:8], tmpl.ID)
	binary.LittleEndian.PutUint16(p[8:], uint16(len(slots)))
	off := 10
	for _, s := range slots {
		binary.LittleEndian.PutUint32(p[off:], uint32(s.Offset))
		binary.LittleEndian.PutUint32(p[off+4:], uint32(s.Length))
		off += 8
	}
	copy(p[off:], tmpl.base)
	return wire.Msg{Type: wire.MsgTemplateDefine, Payload: p}
}

func makeTemplateApply(id uint64, params [][]byte) wire.Msg {
	total := 10
	for _, p := range params {
		total += 2 + len(p)
	}
	buf := make([]byte, total)
	binary.LittleEndian.PutUint64(buf[:8], id)
	binary.LittleEndian.PutUint16(buf[8:], uint16(len(params)))
	off := 10
	for _, p := range params {
		binary.LittleEndian.PutUint16(buf[off:], uint16(len(p)))
		copy(buf[off+2:], p)
		off += 2 + len(p)
	}
	return wire.Msg{Type: wire.MsgTemplateApply, Payload: buf}
}

package compress

import (
	"encoding/binary"

	"james.id.au/proxxxy/internal/wire"
)

// Encoder wraps Dict + TemplateRegistry + RegionTracker and applies all three
// compression levels to outgoing X11 command sequences.
type Encoder struct {
	connID    uint32
	dict      *Dict
	templates *TemplateRegistry
	regions   *RegionTracker
	prev      map[byte][]byte // last command seen per opcode, for template detection
	Stats     *Stats
}

func NewEncoder(connID uint32, dictCapacity int) *Encoder {
	return &Encoder{
		connID:    connID,
		dict:      NewDict(dictCapacity),
		templates: NewTemplateRegistry(),
		regions:   NewRegionTracker(),
		prev:      make(map[byte][]byte),
		Stats:     &Stats{},
	}
}

// OnClientDisconnect resets the client-side view of the dictionary.
func (e *Encoder) OnClientDisconnect() {
	e.dict.ClientDisconnected()
}

// Encode processes one X11 command and returns a sequence of wire messages to
// send to the proxxxy-client. windowID is the target window (0 if not a draw cmd).
func (e *Encoder) Encode(windowID uint32, cmd []byte, order binary.ByteOrder) []wire.Msg {
	if len(cmd) == 0 {
		return nil
	}

	// Level 5: region coalescing — queue and flush.
	if windowID != 0 {
		e.regions.Add(windowID, cmd, order)
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
		p := make([]byte, 12)
		binary.LittleEndian.PutUint32(p[:4], e.connID)
		binary.LittleEndian.PutUint64(p[4:], id)
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
				e.Stats.TemplateDefs.Add(1)
				return []wire.Msg{
					makeTemplateDefine(e.connID, tmpl),
					makeTemplateApply(e.connID, tmpl.ID, params),
				}
			}
			e.Stats.TemplateHits.Add(1)
			return []wire.Msg{makeTemplateApply(e.connID, tmpl.ID, params)}
		}
	}
	e.prev[opcode] = cmd

	// Level 2: dictionary.
	action, id, data := e.dict.Classify(cmd)
	switch action {
	case ActionDefine:
		e.Stats.DictDefines.Add(1)
		return []wire.Msg{makeDictDefine(e.connID, id, data)}
	case ActionRef:
		e.Stats.DictHits.Add(1)
		return []wire.Msg{makeDictRef(e.connID, id)}
	}
	// ActionPassthrough — sequence too large for dict.
	e.Stats.DictPasses.Add(1)
	p := make([]byte, 4+len(cmd))
	binary.LittleEndian.PutUint32(p[:4], e.connID)
	copy(p[4:], cmd)
	return []wire.Msg{{Type: wire.MsgX11Data, Payload: p}}
}

func makeDictDefine(connID uint32, id uint64, data []byte) wire.Msg {
	p := make([]byte, 4+8+len(data))
	binary.LittleEndian.PutUint32(p[:4], connID)
	binary.LittleEndian.PutUint64(p[4:], id)
	copy(p[12:], data)
	return wire.Msg{Type: wire.MsgDictDefine, Payload: p}
}

func makeDictRef(connID uint32, id uint64) wire.Msg {
	p := make([]byte, 4+8)
	binary.LittleEndian.PutUint32(p[:4], connID)
	binary.LittleEndian.PutUint64(p[4:], id)
	return wire.Msg{Type: wire.MsgDictRef, Payload: p}
}

func makeTemplateDefine(connID uint32, tmpl *Template) wire.Msg {
	slots := tmpl.Slots()
	p := make([]byte, 4+8+2+len(slots)*8+len(tmpl.base))
	binary.LittleEndian.PutUint32(p[:4], connID)
	binary.LittleEndian.PutUint64(p[4:], tmpl.ID)
	binary.LittleEndian.PutUint16(p[12:], uint16(len(slots)))
	off := 14
	for _, s := range slots {
		binary.LittleEndian.PutUint32(p[off:], uint32(s.Offset))
		binary.LittleEndian.PutUint32(p[off+4:], uint32(s.Length))
		off += 8
	}
	copy(p[off:], tmpl.base)
	return wire.Msg{Type: wire.MsgTemplateDefine, Payload: p}
}

func makeTemplateApply(connID uint32, id uint64, params [][]byte) wire.Msg {
	total := 4 + 10 // connID + id(8) + nparams(2)
	for _, p := range params {
		total += 2 + len(p)
	}
	buf := make([]byte, total)
	binary.LittleEndian.PutUint32(buf[:4], connID)
	binary.LittleEndian.PutUint64(buf[4:], id)
	binary.LittleEndian.PutUint16(buf[12:], uint16(len(params)))
	off := 14
	for _, p := range params {
		binary.LittleEndian.PutUint16(buf[off:], uint16(len(p)))
		copy(buf[off+2:], p)
		off += 2 + len(p)
	}
	return wire.Msg{Type: wire.MsgTemplateApply, Payload: buf}
}

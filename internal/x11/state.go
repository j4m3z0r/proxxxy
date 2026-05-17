package x11

import (
	"encoding/binary"
	"sync"
)

// Window represents a tracked X11 window.
type Window struct {
	ID          uint32
	Parent      uint32
	X, Y        int16
	Width       uint16
	Height      uint16
	BorderWidth uint16
	Depth       byte
	Class       uint16
	Visual      uint32
	Mapped      bool
	Children    []uint32
	createReq   []byte
}

// GC represents a tracked X11 graphics context.
type GC struct {
	ID         uint32
	Drawable   uint32
	createReq  []byte
	ChangeCmds [][]byte
}

// Pixmap represents a tracked X11 pixmap plus its drawing history.
type Pixmap struct {
	ID       uint32
	Width    uint16
	Height   uint16
	Depth    byte
	DrawCmds [][]byte
}

// Font represents an open X11 font.
type Font struct {
	ID   uint32
	Name string
}

// AppConn is the per-application-connection state maintained by the server.
type AppConn struct {
	ID    uint32
	Order ByteOrder

	mu      sync.RWMutex
	windows map[uint32]*Window
	gcs     map[uint32]*GC
	pixmaps map[uint32]*Pixmap
	fonts   map[uint32]*Font
	seqNum  uint32
}

func NewAppConn(id uint32, order ByteOrder) *AppConn {
	return &AppConn{
		ID:      id,
		Order:   order,
		windows: make(map[uint32]*Window),
		gcs:     make(map[uint32]*GC),
		pixmaps: make(map[uint32]*Pixmap),
		fonts:   make(map[uint32]*Font),
	}
}

// ProcessRequest updates state based on one complete X11 request byte slice.
func (a *AppConn) ProcessRequest(req []byte) {
	if len(req) < 4 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seqNum++
	opcode := req[0]
	switch opcode {
	case OpcodeCreateWindow:
		a.handleCreateWindow(req)
	case OpcodeMapWindow:
		a.handleMapWindow(req)
	case OpcodeUnmapWindow:
		a.handleUnmapWindow(req)
	case OpcodeDestroyWindow:
		a.handleDestroyWindow(req)
	case OpcodeCreateGC:
		a.handleCreateGC(req)
	case OpcodeChangeGC:
		a.handleChangeGC(req)
	case OpcodeFreeGC:
		a.handleFreeGC(req)
	case OpcodeCreatePixmap:
		a.handleCreatePixmap(req)
	case OpcodeFreePixmap:
		a.handleFreePixmap(req)
	case OpcodeOpenFont:
		a.handleOpenFont(req)
	case OpcodeCloseFont:
		a.handleCloseFont(req)
	default:
		a.maybeTrackDrawCmd(opcode, req)
	}
}

func (a *AppConn) handleCreateWindow(req []byte) {
	if len(req) < 32 {
		return
	}
	cp := make([]byte, len(req))
	copy(cp, req)
	w := &Window{
		ID:          U32(req, 4, a.Order),
		Parent:      U32(req, 8, a.Order),
		X:           int16(U16(req, 12, a.Order)),
		Y:           int16(U16(req, 14, a.Order)),
		Width:       U16(req, 16, a.Order),
		Height:      U16(req, 18, a.Order),
		BorderWidth: U16(req, 20, a.Order),
		Class:       U16(req, 22, a.Order),
		Visual:      U32(req, 24, a.Order),
		Depth:       req[1],
		createReq:   cp,
	}
	a.windows[w.ID] = w
	if p, ok := a.windows[w.Parent]; ok {
		p.Children = append(p.Children, w.ID)
	}
}

func (a *AppConn) handleMapWindow(req []byte) {
	if len(req) < 8 {
		return
	}
	id := U32(req, 4, a.Order)
	if w, ok := a.windows[id]; ok {
		w.Mapped = true
	}
}

func (a *AppConn) handleUnmapWindow(req []byte) {
	if len(req) < 8 {
		return
	}
	id := U32(req, 4, a.Order)
	if w, ok := a.windows[id]; ok {
		w.Mapped = false
	}
}

func (a *AppConn) handleDestroyWindow(req []byte) {
	if len(req) < 8 {
		return
	}
	id := U32(req, 4, a.Order)
	if w, ok := a.windows[id]; ok {
		if p, ok2 := a.windows[w.Parent]; ok2 {
			p.Children = removeID(p.Children, id)
		}
	}
	delete(a.windows, id)
}

func (a *AppConn) handleCreateGC(req []byte) {
	if len(req) < 16 {
		return
	}
	cp := make([]byte, len(req))
	copy(cp, req)
	gc := &GC{
		ID:        U32(req, 4, a.Order),
		Drawable:  U32(req, 8, a.Order),
		createReq: cp,
	}
	a.gcs[gc.ID] = gc
}

func (a *AppConn) handleChangeGC(req []byte) {
	if len(req) < 12 {
		return
	}
	id := U32(req, 4, a.Order)
	if gc, ok := a.gcs[id]; ok {
		cp := make([]byte, len(req))
		copy(cp, req)
		gc.ChangeCmds = append(gc.ChangeCmds, cp)
	}
}

func (a *AppConn) handleFreeGC(req []byte) {
	if len(req) < 8 {
		return
	}
	delete(a.gcs, U32(req, 4, a.Order))
}

func (a *AppConn) handleCreatePixmap(req []byte) {
	// X11 CreatePixmap layout: [opcode:1][depth:1][length:2][pid:4][drawable:4][width:2][height:2]
	if len(req) < 16 {
		return
	}
	a.pixmaps[U32(req, 4, a.Order)] = &Pixmap{
		ID:     U32(req, 4, a.Order),
		Depth:  req[1],
		Width:  U16(req, 12, a.Order),
		Height: U16(req, 14, a.Order),
	}
}

func (a *AppConn) handleFreePixmap(req []byte) {
	if len(req) < 8 {
		return
	}
	delete(a.pixmaps, U32(req, 4, a.Order))
}

func (a *AppConn) handleOpenFont(req []byte) {
	if len(req) < 12 {
		return
	}
	fid := U32(req, 4, a.Order)
	nameLen := int(U16(req, 8, a.Order))
	name := ""
	if len(req) >= 12+nameLen {
		name = string(req[12 : 12+nameLen])
	}
	a.fonts[fid] = &Font{ID: fid, Name: name}
}

func (a *AppConn) handleCloseFont(req []byte) {
	if len(req) < 8 {
		return
	}
	delete(a.fonts, U32(req, 4, a.Order))
}

var drawTargetOpcodes = map[byte]bool{
	OpcodeClearArea: true, OpcodeCopyArea: true, OpcodeCopyPlane: true,
	OpcodePolyPoint: true, OpcodePolyLine: true, OpcodePolySegment: true,
	OpcodePolyRectangle: true, OpcodePolyArc: true, OpcodeFillPoly: true,
	OpcodePolyFillRectangle: true, OpcodePolyFillArc: true,
	OpcodePutImage: true, OpcodePolyText8: true, OpcodePolyText16: true,
	OpcodeImageText8: true, OpcodeImageText16: true,
}

func (a *AppConn) maybeTrackDrawCmd(opcode byte, req []byte) {
	if !drawTargetOpcodes[opcode] || len(req) < 8 {
		return
	}
	drawable := U32(req, 4, a.Order)
	if pm, ok := a.pixmaps[drawable]; ok {
		cp := make([]byte, len(req))
		copy(cp, req)
		pm.DrawCmds = append(pm.DrawCmds, cp)
	}
}

// Window returns the window with the given ID (nil if not found).
func (a *AppConn) Window(id uint32) *Window {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.windows[id]
}

// GC returns the GC with the given ID (nil if not found).
func (a *AppConn) GC(id uint32) *GC {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.gcs[id]
}

// Windows returns a snapshot of all tracked windows.
func (a *AppConn) Windows() map[uint32]*Window {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[uint32]*Window, len(a.windows))
	for k, v := range a.windows {
		out[k] = v
	}
	return out
}

func removeID(s []uint32, id uint32) []uint32 {
	out := s[:0]
	for _, v := range s {
		if v != id {
			out = append(out, v)
		}
	}
	return out
}

// byteOrderFromByte returns the byte order for the X11 connection setup byte.
func byteOrderFromByte(b byte) ByteOrder {
	if b == 0x42 {
		return binary.BigEndian
	}
	return binary.LittleEndian
}

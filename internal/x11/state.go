package x11

import (
	"log"
	"sync"
)

// Window represents a tracked X11 window.
type Window struct {
	ID               uint32
	Parent           uint32
	X, Y             int16
	Width            uint16
	Height           uint16
	BorderWidth      uint16
	Depth            byte
	Class            uint16
	Visual           uint32
	Mapped           bool
	Children         []uint32
	EventMask        uint32 // current XSelectInput event mask (CreateWindow + ChangeWindowAttributes)
	PendingConfigures uint32 // ConfigureWindow requests sent since last DrainPendingConfigures
	PropertyCmds     [][]byte // raw ChangeProperty request bytes in order, replayed during synthesis
	Xi2Masks         [][]byte // raw XISelectEvents request bytes in order, replayed during synthesis
	createReq        []byte
}

// GC represents a tracked X11 graphics context.
type GC struct {
	ID            uint32
	Drawable      uint32
	DrawableDepth byte // depth of the drawable at GC creation time (0 = unknown)
	createReq     []byte
	ChangeCmds    [][]byte
}

// Pixmap represents a tracked X11 pixmap plus its drawing history.
type Pixmap struct {
	ID        uint32
	Width     uint16
	Height    uint16
	Depth     byte
	DrawCmds  [][]byte
	createReq []byte
}

// Font represents an open X11 font.
type Font struct {
	ID      uint32
	Name    string
	openReq []byte
}

// OpenReq returns the raw X11 OpenFont request bytes.
func (f Font) OpenReq() []byte { return f.openReq }

// Cursor represents a tracked X11 cursor (CreateCursor or CreateGlyphCursor).
type Cursor struct {
	ID          uint32
	createReq   []byte // raw CreateCursor or CreateGlyphCursor request
	srcFontName string // for CreateGlyphCursor: name of source font at creation time
	renderPicReq []byte // for RenderCreateCursor: saved source picture create request
}

// CreateReq returns the raw cursor-creation request bytes.
func (c Cursor) CreateReq() []byte { return c.createReq }

// SrcFontName returns the name of the source font used by CreateGlyphCursor
// (empty string for CreateCursor or if the font name was not available).
func (c Cursor) SrcFontName() string { return c.srcFontName }

// RenderPicReq returns the saved source picture creation request for
// RenderCreateCursor cursors (empty for core cursors). Used during synthesis
// to recreate the source picture when it has already been freed.
func (c Cursor) RenderPicReq() []byte { return c.renderPicReq }

// Picture represents a RENDER extension Picture resource.
type Picture struct {
	ID         uint32
	Drawable   uint32 // window or pixmap that backs this picture
	createReq  []byte // raw RenderCreatePicture request bytes
	ChangeCmds [][]byte
}

// CreateReq returns the raw RenderCreatePicture request bytes.
func (p Picture) CreateReq() []byte { return p.createReq }

// GlyphSet represents a RENDER extension GlyphSet resource.
// GlyphSets use IDs from the app's resource-id space but their lifetime is
// independent of the creating connection; however, they are freed when the
// last referencing connection closes. Synthesis must recreate them.
type GlyphSet struct {
	ID            uint32
	createReq     []byte   // raw RenderCreateGlyphSet or RenderReferenceGlyphSet request
	AddGlyphsCmds [][]byte // replay log of RenderAddGlyphs requests
}

// CreateReq returns the raw RenderCreateGlyphSet (or ReferenceGlyphSet) request bytes.
func (g GlyphSet) CreateReq() []byte { return g.createReq }

// AppConn is the per-application-connection state maintained by the server.
type AppConn struct {
	ID    uint32
	Order ByteOrder

	mu         sync.RWMutex
	windows    map[uint32]*Window
	gcs        map[uint32]*GC
	pixmaps    map[uint32]*Pixmap
	fonts      map[uint32]*Font
	fontNames  map[uint32]string // permanent registry: font ID → name, never deleted on CloseFont
	cursors    map[uint32]*Cursor
	seqNum     uint32
	setupReq   []byte // X11 connection setup request from app
	ridBase    uint32 // resource-id-base from real X server setup reply
	pictures   map[uint32]*Picture
	glyphSets  map[uint32]*GlyphSet
	ridMask    uint32 // resource-id-mask from real X server setup reply
	shmSegs     map[uint32][]byte // shmseg resource ID → raw ShmAttach request bytes
	shmPixmaps  map[uint32][]byte // pixmap resource ID → raw ShmCreatePixmap request bytes
	syncCounters map[uint32][]byte // counter resource ID → raw SyncCreateCounter request bytes
	// dynamicRenderOpcode is the RENDER major opcode as assigned by the real X
	// server. X.org assigns opcode 139 by convention, but XQuartz uses 141. We
	// learn the actual value from the first RenderCreatePicture request the app
	// sends (req[1] == RenderCreatePicture minor opcode == 4, len >= 20). Zero
	// means not yet learned; fall back to OpcodeRender (139) in that case.
	dynamicRenderOpcode byte

	// Extension major opcodes are server-assigned and NOT fixed: X.org and XQuartz
	// disagree (e.g. XKEYBOARD is 135 on X.org but 136 on XQuartz). We learn the
	// real opcode for each extension by pairing QueryExtension requests with their
	// replies. pendingQueryExt maps a QueryExtension request's sequence number to
	// the extension name; extByOpcode maps the learned major opcode to the name.
	// Used so the disconnect-window fake-reply logic knows which extension a
	// dropped request belongs to (see ExtNeedsReply).
	pendingQueryExt map[uint16]string
	extByOpcode     map[byte]string
}

func NewAppConn(id uint32, order ByteOrder) *AppConn {
	return &AppConn{
		ID:        id,
		Order:     order,
		windows:   make(map[uint32]*Window),
		gcs:       make(map[uint32]*GC),
		pixmaps:   make(map[uint32]*Pixmap),
		fonts:     make(map[uint32]*Font),
		fontNames: make(map[uint32]string),
		cursors:   make(map[uint32]*Cursor),
		pictures:  make(map[uint32]*Picture),
		glyphSets: make(map[uint32]*GlyphSet),
		shmSegs:      make(map[uint32][]byte),
		shmPixmaps:   make(map[uint32][]byte),
		syncCounters: make(map[uint32][]byte),
		pendingQueryExt: make(map[uint16]string),
		extByOpcode:     make(map[byte]string),
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
	case OpcodeChangeWindowAttributes:
		a.handleChangeWindowAttributes(req)
	case OpcodeChangeProperty:
		a.handleChangeProperty(req)
	case OpcodeConfigureWindow:
		a.handleConfigureWindow(req)
	case OpcodeMapWindow:
		a.handleMapWindow(req)
	case OpcodeMapSubwindows:
		a.handleMapSubwindows(req)
	case OpcodeUnmapWindow:
		a.handleUnmapWindow(req)
	case OpcodeDestroyWindow:
		a.handleDestroyWindow(req)
	case OpcodeDestroySubwindows:
		a.handleDestroySubwindows(req)
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
	case OpcodeCreateCursor, OpcodeCreateGlyphCursor:
		a.handleCreateCursor(req)
	case OpcodeFreeCursor:
		a.handleFreeCursor(req)
	case OpcodeRender:
		a.handleRender(req)
	case OpcodeXInput:
		a.handleXInput(req)
	case OpcodeMITSHM:
		a.handleMITSHM(req)
	case OpcodeSYNC:
		a.handleSYNC(req)
	case OpcodeQueryExtension:
		// [98][pad][len:2][nameLen:2][pad:2][name...]. Record seq->name so the
		// reply (parsed in the server relay) can teach us the extension's opcode.
		if len(req) >= 8 {
			nameLen := int(a.Order.Uint16(req[4:6]))
			if len(req) >= 8+nameLen {
				a.pendingQueryExt[uint16(a.seqNum)] = string(req[8 : 8+nameLen])
			}
		}
	default:
		// Extension major opcodes are NOT fixed: XQuartz assigns SYNC=135,
		// MIT-SHM=132, XInput=133 (vs X.org's 134/130/131). Route by the opcode
		// learned from QueryExtension (extByOpcode) so extension state is tracked
		// on every server. This is critical for SYNC: GTK's frame clock issues
		// SyncSetCounter on a counter it created; if proxxxy doesn't track the
		// SyncCreateCounter (wrong opcode), synthesis can't recreate the counter,
		// and after reconnect SyncSetCounter hits a dead counter → BadCounter →
		// the app crashes.
		switch a.extByOpcode[opcode] {
		case "SYNC":
			a.handleSYNC(req)
			return
		case "MIT-SHM":
			a.handleMITSHM(req)
			return
		case "XInputExtension":
			a.handleXInput(req)
			return
		}
		// Dynamically detect the RENDER extension: when the first
		// RenderCreatePicture (minor=4, len>=20) arrives with any extension
		// opcode, record it as the RENDER opcode and route all subsequent requests
		// with that opcode to handleRender. (RENDER is also covered by extByOpcode
		// once QueryExtension is seen, but this catches it even earlier.)
		if opcode >= 128 && len(req) >= 4 {
			minor := req[1]
			if a.dynamicRenderOpcode == 0 && minor == RenderCreatePicture && len(req) >= 20 {
				reqLen := a.Order.Uint16(req[2:4])
				if reqLen >= 5 {
					a.dynamicRenderOpcode = opcode
					log.Printf("x11: conn %d learned dynamic RENDER opcode=%d", a.ID, opcode)
				}
			}
			if a.dynamicRenderOpcode != 0 && opcode == a.dynamicRenderOpcode {
				a.handleRender(req)
				return
			}
		}
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
	// Extract CWEventMask (bit 11) from the value-list if present.
	valueMask := U32(req, 28, a.Order)
	if valueMask&(1<<11) != 0 {
		off := 32
		for b := uint(0); b < 11; b++ {
			if valueMask&(1<<b) != 0 {
				off += 4
			}
		}
		if len(req) >= off+4 {
			w.EventMask = U32(req, off, a.Order)
		}
	}
	a.windows[w.ID] = w
	if p, ok := a.windows[w.Parent]; ok {
		p.Children = append(p.Children, w.ID)
	}
}

func (a *AppConn) handleConfigureWindow(req []byte) {
	// Layout: [opcode:1][pad:1][len:2][window:4][value-mask:2][pad:2][values...]
	if len(req) < 12 {
		return
	}
	wid := U32(req, 4, a.Order)
	w, ok := a.windows[wid]
	if !ok {
		return
	}
	mask := uint32(a.Order.Uint16(req[8:10]))
	off := 0
	val := func() uint32 {
		v := U32(req, 12+off*4, a.Order)
		off++
		return v
	}
	if mask&(1<<0) != 0 { w.X = int16(val()) }
	if mask&(1<<1) != 0 { w.Y = int16(val()) }
	if mask&(1<<2) != 0 { w.Width = uint16(val()) }
	if mask&(1<<3) != 0 { w.Height = uint16(val()) }
	if mask&(1<<4) != 0 { w.BorderWidth = uint16(val()) }
	// Patch createReq so synthesis replays the correct geometry.
	if len(w.createReq) >= 20 {
		a.Order.PutUint16(w.createReq[12:], uint16(w.X))
		a.Order.PutUint16(w.createReq[14:], uint16(w.Y))
		a.Order.PutUint16(w.createReq[16:], w.Width)
		a.Order.PutUint16(w.createReq[18:], w.Height)
		a.Order.PutUint16(w.createReq[20:], w.BorderWidth)
	}
	w.PendingConfigures++
}

func (a *AppConn) handleChangeWindowAttributes(req []byte) {
	// Layout: [opcode:1][pad:1][length:2][window:4][value-mask:4][values...]
	if len(req) < 12 {
		return
	}
	wid := U32(req, 4, a.Order)
	w, ok := a.windows[wid]
	if !ok {
		return
	}
	valueMask := U32(req, 8, a.Order)
	if valueMask&(1<<11) == 0 { // no CWEventMask — nothing to track
		return
	}
	off := 12
	for b := uint(0); b < 11; b++ {
		if valueMask&(1<<b) != 0 {
			off += 4
		}
	}
	if len(req) >= off+4 {
		w.EventMask = U32(req, off, a.Order)
	}
}

func (a *AppConn) handleXInput(req []byte) {
	// XISelectEvents layout: [major:1][minor:1][len:2][window:4][num_masks:2][pad:2][masks...]
	if len(req) < 12 || req[1] != XISelectEvents {
		return
	}
	winID := U32(req, 4, a.Order)
	w, ok := a.windows[winID]
	if !ok {
		return
	}
	cp := make([]byte, len(req))
	copy(cp, req)
	w.Xi2Masks = append(w.Xi2Masks, cp)
}

func (a *AppConn) handleChangeProperty(req []byte) {
	// Layout: [opcode:1][mode:1][len:2][window:4][property-atom:4][type-atom:4][format:1][pad:3][data-len:4][data...]
	if len(req) < 24 {
		return
	}
	wid := U32(req, 4, a.Order)
	w, ok := a.windows[wid]
	if !ok {
		return
	}
	cp := make([]byte, len(req))
	copy(cp, req)
	// Replace (mode=0) overwrites same-property entries to avoid stale values;
	// Append/Prepend (mode=1/2) just accumulate.
	if req[1] == 0 {
		prop := U32(req, 8, a.Order)
		for i, prev := range w.PropertyCmds {
			if len(prev) >= 12 && U32(prev, 8, a.Order) == prop {
				w.PropertyCmds[i] = cp
				return
			}
		}
	}
	w.PropertyCmds = append(w.PropertyCmds, cp)
}

func (a *AppConn) handleMapWindow(req []byte) {
	if len(req) < 8 {
		return
	}
	id := U32(req, 4, a.Order)
	if w, ok := a.windows[id]; ok {
		log.Printf("x11: conn %d MapWindow 0x%08x (was mapped=%v)", a.ID, id, w.Mapped)
		w.Mapped = true
	} else {
		log.Printf("x11: conn %d MapWindow 0x%08x (unknown window)", a.ID, id)
	}
}

func (a *AppConn) handleMapSubwindows(req []byte) {
	if len(req) < 8 {
		return
	}
	parentID := U32(req, 4, a.Order)
	log.Printf("x11: conn %d MapSubwindows 0x%08x", a.ID, parentID)
	if w, ok := a.windows[parentID]; ok {
		for _, childID := range w.Children {
			if child, ok2 := a.windows[childID]; ok2 {
				log.Printf("x11: conn %d MapSubwindows -> MapWindow 0x%08x", a.ID, childID)
				child.Mapped = true
			}
		}
	}
}

func (a *AppConn) handleUnmapWindow(req []byte) {
	if len(req) < 8 {
		return
	}
	id := U32(req, 4, a.Order)
	if w, ok := a.windows[id]; ok {
		log.Printf("x11: conn %d UnmapWindow 0x%08x (was mapped=%v)", a.ID, id, w.Mapped)
		w.Mapped = false
	}
}

func (a *AppConn) handleDestroyWindow(req []byte) {
	if len(req) < 8 {
		return
	}
	a.destroyWindowTree(U32(req, 4, a.Order))
}

func (a *AppConn) handleDestroySubwindows(req []byte) {
	if len(req) < 8 {
		return
	}
	parentID := U32(req, 4, a.Order)
	parent, ok := a.windows[parentID]
	if !ok {
		return
	}
	for _, childID := range append([]uint32(nil), parent.Children...) {
		a.destroyWindowTree(childID)
	}
}

// destroyWindowTree removes a window and all its descendants from a.windows.
// X11 DestroyWindow destroys the entire subtree; without cascading here, orphan
// children remain in the map with a stale parent, causing BadWindow during
// synthesis when findRoots treats them as roots and tries to create them with
// a parent that no longer exists.
func (a *AppConn) destroyWindowTree(id uint32) {
	w, ok := a.windows[id]
	if !ok {
		return
	}
	// Copy children slice before recursing — the recursive calls modify Children.
	children := append([]uint32(nil), w.Children...)
	for _, childID := range children {
		a.destroyWindowTree(childID)
	}
	if p, ok2 := a.windows[w.Parent]; ok2 {
		p.Children = removeID(p.Children, id)
	}
	delete(a.windows, id)
}

func (a *AppConn) handleCreateGC(req []byte) {
	if len(req) < 16 {
		return
	}
	cp := make([]byte, len(req))
	copy(cp, req)
	drawable := U32(req, 8, a.Order)
	gc := &GC{
		ID:        U32(req, 4, a.Order),
		Drawable:  drawable,
		createReq: cp,
	}
	// Record the drawable's depth so synthesis can find a matching fallback
	// drawable if the original is gone at reconnect time.
	if pm, ok := a.pixmaps[drawable]; ok {
		gc.DrawableDepth = pm.Depth
	} else if w, ok := a.windows[drawable]; ok {
		gc.DrawableDepth = w.Depth
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
	id := U32(req, 4, a.Order)
	delete(a.gcs, id)
}

func (a *AppConn) handleCreatePixmap(req []byte) {
	// X11 CreatePixmap layout: [opcode:1][depth:1][length:2][pid:4][drawable:4][width:2][height:2]
	if len(req) < 16 {
		return
	}
	cp := make([]byte, len(req))
	copy(cp, req)
	a.pixmaps[U32(req, 4, a.Order)] = &Pixmap{
		ID:        U32(req, 4, a.Order),
		Depth:     req[1],
		Width:     U16(req, 12, a.Order),
		Height:    U16(req, 14, a.Order),
		createReq: cp,
	}
}

func (a *AppConn) handleFreePixmap(req []byte) {
	if len(req) < 8 {
		return
	}
	id := U32(req, 4, a.Order)
	delete(a.pixmaps, id)
	delete(a.shmPixmaps, id)
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
	cp := make([]byte, len(req))
	copy(cp, req)
	a.fonts[fid] = &Font{ID: fid, Name: name, openReq: cp}
	// Persist the name even after CloseFont so CreateGlyphCursor requests that
	// arrive after the font is closed can still recover the name for synthesis.
	a.fontNames[fid] = name
}

func (a *AppConn) handleCloseFont(req []byte) {
	if len(req) < 8 {
		return
	}
	delete(a.fonts, U32(req, 4, a.Order))
}

func (a *AppConn) handleCreateCursor(req []byte) {
	// Both CreateCursor and CreateGlyphCursor have cid at [4:8].
	if len(req) < 16 {
		return
	}
	cid := U32(req, 4, a.Order)
	cp := make([]byte, len(req))
	copy(cp, req)
	cur := &Cursor{ID: cid, createReq: cp}
	// For CreateGlyphCursor, save the source font name using the permanent
	// registry (fontNames). The font may have been closed already (or may be
	// closed later), but we still need the name to reopen it during synthesis.
	if req[0] == OpcodeCreateGlyphCursor {
		srcFontID := U32(req, 8, a.Order)
		if name, ok := a.fontNames[srcFontID]; ok {
			cur.srcFontName = name
		}
	}
	a.cursors[cid] = cur
}

func (a *AppConn) handleFreeCursor(req []byte) {
	if len(req) < 8 {
		return
	}
	delete(a.cursors, U32(req, 4, a.Order))
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

// Windows returns a snapshot of all tracked windows with deep-copied slices.
func (a *AppConn) Windows() map[uint32]Window {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[uint32]Window, len(a.windows))
	for k, v := range a.windows {
		w := *v                                            // copy struct
		w.Children = append([]uint32(nil), v.Children...) // deep copy slice
		w.Xi2Masks = append([][]byte(nil), v.Xi2Masks...) // shallow copy outer slice
		out[k] = w
	}
	return out
}

// GCs returns a snapshot of all tracked graphics contexts.
func (a *AppConn) GCs() map[uint32]GC {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[uint32]GC, len(a.gcs))
	for k, v := range a.gcs {
		out[k] = *v // copy struct (ChangeCmds is a slice but synthesis doesn't use it)
	}
	return out
}

// Pixmaps returns a snapshot of all tracked pixmaps with deep-copied slices.
func (a *AppConn) Pixmaps() map[uint32]Pixmap {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[uint32]Pixmap, len(a.pixmaps))
	for k, v := range a.pixmaps {
		p := *v                                          // copy struct
		p.DrawCmds = append([][]byte(nil), v.DrawCmds...) // deep copy slice of slices
		out[k] = p
	}
	return out
}

// Cursors returns a snapshot of all currently tracked cursors.
func (a *AppConn) Cursors() map[uint32]Cursor {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[uint32]Cursor, len(a.cursors))
	for k, v := range a.cursors {
		out[k] = *v
	}
	return out
}

// Fonts returns a snapshot of all currently open fonts.
func (a *AppConn) Fonts() map[uint32]Font {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[uint32]Font, len(a.fonts))
	for k, v := range a.fonts {
		out[k] = *v
	}
	return out
}

// SeqNum returns the number of requests the app has sent on this connection.
func (a *AppConn) SeqNum() uint32 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.seqNum
}

// SetSetupReq stores the raw X11 connection setup request sent by the app.
func (a *AppConn) SetSetupReq(b []byte) {
	a.mu.Lock()
	a.setupReq = b
	a.mu.Unlock()
}

// SetupReq returns the stored connection setup request bytes.
func (a *AppConn) SetupReq() []byte {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.setupReq
}

// SetRID records the resource-id-base and resource-id-mask from the real X server.
func (a *AppConn) SetRID(base, mask uint32) {
	a.mu.Lock()
	a.ridBase = base
	a.ridMask = mask
	a.mu.Unlock()
}

// RID returns the stored resource-id-base and resource-id-mask.
func (a *AppConn) RID() (base, mask uint32) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ridBase, a.ridMask
}

// CreateReq returns the raw X11 CreateWindow request bytes.
func (w Window) CreateReq() []byte { return w.createReq }

// CreateReq returns the raw X11 CreateGC request bytes.
func (g GC) CreateReq() []byte { return g.createReq }

// CreateReq returns the raw X11 CreatePixmap request bytes.
func (p Pixmap) CreateReq() []byte { return p.createReq }

func (a *AppConn) handleRender(req []byte) {
	if len(req) < 8 {
		return
	}
	switch req[1] { // RENDER minor opcode
	case RenderCreatePicture:
		// Layout: [139:1][4:1][len:2][pid:4][drawable:4][format:4][value-mask:4]...
		if len(req) < 16 {
			return
		}
		pid := a.Order.Uint32(req[4:8])
		drawable := a.Order.Uint32(req[8:12])
		cp := make([]byte, len(req))
		copy(cp, req)
		a.pictures[pid] = &Picture{ID: pid, Drawable: drawable, createReq: cp}
	case RenderCreateSolidFill, RenderCreateLinearGradient,
		RenderCreateRadialGradient, RenderCreateConicalGradient:
		// Layout: [139:1][minor:1][len:2][pid:4][...gradient/color data...]
		// No backing drawable; pid is the only resource reference.
		if len(req) < 8 {
			return
		}
		pid := a.Order.Uint32(req[4:8])
		cp := make([]byte, len(req))
		copy(cp, req)
		a.pictures[pid] = &Picture{ID: pid, Drawable: 0, createReq: cp}
	case RenderChangePicture:
		// Layout: [139:1][5:1][len:2][pid:4][value-mask:4]...
		pid := a.Order.Uint32(req[4:8])
		if p, ok := a.pictures[pid]; ok {
			cp := make([]byte, len(req))
			copy(cp, req)
			p.ChangeCmds = append(p.ChangeCmds, cp)
		}
	case RenderFreePicture:
		// Layout: [139:1][7:1][len:2][pid:4]
		delete(a.pictures, a.Order.Uint32(req[4:8]))
	case RenderCreateGlyphSet:
		// Layout: [139:1][16:1][len:2][gsid:4][format:4]
		if len(req) < 12 {
			return
		}
		gsid := a.Order.Uint32(req[4:8])
		cp := make([]byte, len(req))
		copy(cp, req)
		a.glyphSets[gsid] = &GlyphSet{ID: gsid, createReq: cp}
	case RenderReferenceGlyphSet:
		// Layout: [139:1][17:1][len:2][gsid_new:4][gsid_existing:4]
		if len(req) < 12 {
			return
		}
		gsid := a.Order.Uint32(req[4:8])
		cp := make([]byte, len(req))
		copy(cp, req)
		a.glyphSets[gsid] = &GlyphSet{ID: gsid, createReq: cp}
	case RenderFreeGlyphSet:
		// Layout: [139:1][18:1][len:2][gsid:4]
		if len(req) < 8 {
			return
		}
		delete(a.glyphSets, a.Order.Uint32(req[4:8]))
	case RenderAddGlyphs:
		// Layout: [139:1][20:1][len:2][gsid:4][nglyphs:4][gids:4*n][infos:12*n][image_data...]
		if len(req) < 12 {
			return
		}
		gsid := a.Order.Uint32(req[4:8])
		if gs, ok := a.glyphSets[gsid]; ok {
			cp := make([]byte, len(req))
			copy(cp, req)
			gs.AddGlyphsCmds = append(gs.AddGlyphsCmds, cp)
		}
	case RenderCreateCursor:
		// Layout: [139:1][27:1][len:2][cid:4][source_picture:4][x:2][y:2] = 12 bytes
		if len(req) < 12 {
			return
		}
		cid := a.Order.Uint32(req[4:8])
		srcPicID := a.Order.Uint32(req[8:12])
		cp := make([]byte, len(req))
		copy(cp, req)
		cur := &Cursor{ID: cid, createReq: cp}
		// Save the source picture's create request so synthesis can recreate it
		// temporarily (Firefox frees the picture immediately after cursor creation).
		if pic, ok := a.pictures[srcPicID]; ok {
			if picReq := pic.CreateReq(); len(picReq) > 0 {
				cur.renderPicReq = picReq
			}
		}
		a.cursors[cid] = cur
	}
}

func (a *AppConn) handleMITSHM(req []byte) {
	if len(req) < 4 {
		return
	}
	switch req[1] { // MIT-SHM minor opcode
	case SHMAttach:
		// Layout: [opcode:1][1:1][len:2][shmseg:4][shmid:4][read-only:1][pad:3]
		if len(req) < 12 {
			return
		}
		seg := a.Order.Uint32(req[4:8])
		cp := make([]byte, len(req))
		copy(cp, req)
		a.shmSegs[seg] = cp
	case SHMDetach:
		// Layout: [opcode:1][2:1][len:2][shmseg:4]
		if len(req) < 8 {
			return
		}
		delete(a.shmSegs, a.Order.Uint32(req[4:8]))
	case SHMCreatePixmap:
		// Layout: [opcode:1][5:1][len:2][pid:4][drawable:4][width:2][height:2][depth:1][pad:3][shmseg:4][offset:4]
		if len(req) < 28 {
			return
		}
		pid := a.Order.Uint32(req[4:8])
		cp := make([]byte, len(req))
		copy(cp, req)
		a.shmPixmaps[pid] = cp
		// Also register as a pixmap for draw-command tracking and GC drawable fallback.
		depth := req[16]
		width := a.Order.Uint16(req[12:14])
		height := a.Order.Uint16(req[14:16])
		a.pixmaps[pid] = &Pixmap{ID: pid, Depth: depth, Width: width, Height: height}
	}
}

func (a *AppConn) handleSYNC(req []byte) {
	if len(req) < 4 {
		return
	}
	switch req[1] { // SYNC minor opcode
	case SYNCCreateCounter:
		// Layout: [opcode:1][2:1][len:2][counter:4][initial-lo:4][initial-hi:4]
		if len(req) < 16 {
			return
		}
		counter := a.Order.Uint32(req[4:8])
		cp := make([]byte, len(req))
		copy(cp, req)
		a.syncCounters[counter] = cp
	case SYNCDestroyCounter:
		// Layout: [opcode:1][6:1][len:2][counter:4]
		if len(req) < 8 {
			return
		}
		delete(a.syncCounters, a.Order.Uint32(req[4:8]))
	}
}

// ShmSegs returns a snapshot of currently-attached MIT-SHM segments.
// Each value is the raw ShmAttach request that created the segment.
func (a *AppConn) ShmSegs() map[uint32][]byte {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[uint32][]byte, len(a.shmSegs))
	for k, v := range a.shmSegs {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

// ShmPixmaps returns a snapshot of currently-alive MIT-SHM pixmaps.
// Each value is the raw ShmCreatePixmap request that created the pixmap.
func (a *AppConn) ShmPixmaps() map[uint32][]byte {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[uint32][]byte, len(a.shmPixmaps))
	for k, v := range a.shmPixmaps {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

// SyncCounters returns a snapshot of currently-alive SYNC counters.
// Each value is the raw SyncCreateCounter request that created the counter.
func (a *AppConn) SyncCounters() map[uint32][]byte {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[uint32][]byte, len(a.syncCounters))
	for k, v := range a.syncCounters {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

// ResetPendingConfigures zeroes the PendingConfigures count for every window.
// Call this at the start of synthesis so that DrainPendingConfigures later
// returns only the count from this reconnect session (not accumulated counts
// from normal operation where ConfigureWindow/ConfigureNotify were balanced).
func (a *AppConn) ResetPendingConfigures() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, w := range a.windows {
		w.PendingConfigures = 0
	}
}

// DrainPendingConfigures returns and resets the PendingConfigures count for each
// window that had at least one outstanding ConfigureWindow request. Call this
// during synthesis to determine how many ConfigureNotify events to inject per
// window — one per pending configure request is needed to drain GTK3's
// configure_request_count (which it increments on ConfigureWindow and
// decrements on ConfigureNotify).
func (a *AppConn) DrainPendingConfigures() map[uint32]uint32 {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[uint32]uint32)
	for id, w := range a.windows {
		if w.PendingConfigures > 0 {
			out[id] = w.PendingConfigures
			w.PendingConfigures = 0
		}
	}
	return out
}

// Pictures returns a snapshot of all tracked RENDER pictures.
func (a *AppConn) Pictures() map[uint32]Picture {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[uint32]Picture, len(a.pictures))
	for k, v := range a.pictures {
		p := Picture{ID: v.ID, Drawable: v.Drawable}
		if len(v.createReq) > 0 {
			p.createReq = append([]byte(nil), v.createReq...)
		}
		for _, cmd := range v.ChangeCmds {
			p.ChangeCmds = append(p.ChangeCmds, append([]byte(nil), cmd...))
		}
		out[k] = p
	}
	return out
}

// GlyphSets returns a snapshot of all tracked RENDER GlyphSets.
func (a *AppConn) GlyphSets() map[uint32]GlyphSet {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[uint32]GlyphSet, len(a.glyphSets))
	for k, v := range a.glyphSets {
		gs := GlyphSet{ID: v.ID}
		if len(v.createReq) > 0 {
			gs.createReq = append([]byte(nil), v.createReq...)
		}
		for _, cmd := range v.AddGlyphsCmds {
			gs.AddGlyphsCmds = append(gs.AddGlyphsCmds, append([]byte(nil), cmd...))
		}
		out[k] = gs
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

// LearnQueryExtensionReply matches a QueryExtension reply (by sequence number)
// to the pending request and records the extension's server-assigned major
// opcode. Called by the server when relaying replies from the real X server.
func (a *AppConn) LearnQueryExtensionReply(seq uint16, present bool, major byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	name, ok := a.pendingQueryExt[seq]
	if !ok {
		return
	}
	delete(a.pendingQueryExt, seq)
	if present && major != 0 {
		a.extByOpcode[major] = name
	}
}

// xkbReplyMinors is the set of XKEYBOARD minor opcodes that elicit a reply.
// GTK apps (GIMP, Firefox, LibreOffice) issue XkbGetMap (8), XkbGetNames (17),
// etc. synchronously; if one lands in a client-disconnect window with no reply,
// Xlib's _XReply blocks and the whole app freezes (window stays black after
// reconnect). Source: xkbproto request list.
var xkbReplyMinors = map[byte]bool{
	0: true, 4: true, 6: true, 8: true, 10: true, 14: true,
	16: true, 17: true, 19: true, 21: true, 24: true,
}

// extReplyMinors maps an extension name to the set of its minor opcodes that
// elicit a reply (only the ones GTK toolkits issue synchronously are needed).
var extReplyMinors = map[string]map[byte]bool{
	"XKEYBOARD": xkbReplyMinors,
	"RENDER":    {0: true, 1: true, 2: true, 29: true}, // QueryVersion/PictFormats/PictIndexValues/QueryFilters
	"SYNC":      {0: true, 1: true, 5: true},           // Initialize/ListSystemCounters/QueryCounter
	"XFIXES":    {0: true},                             // QueryVersion
	"DAMAGE":    {0: true},                             // QueryVersion
	"RANDR":     {0: true},                             // QueryVersion
	"MIT-SHM":   {0: true},                             // QueryVersion
	"XInputExtension": {1: true, 47: true},             // GetExtensionVersion/XIQueryVersion
}

// ExtNeedsReply reports whether an extension request (major opcode + minor)
// expects a synchronous reply, using the opcodes learned via QueryExtension.
// Used by the disconnect-window fake-reply path to avoid hanging apps that
// issue synchronous extension requests (notably XKB) while no client is
// connected. Returns false for unknown extensions/minors (safer not to fake).
func (a *AppConn) ExtNeedsReply(opcode, minor byte) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	name, ok := a.extByOpcode[opcode]
	if !ok {
		return false
	}
	return extReplyMinors[name][minor]
}

// OpcodeNeedsReply reports whether the X11 core-protocol opcode expects a
// synchronous reply from the server. Extension opcodes (≥128) are not covered;
// callers should not inject fake replies for those.
func OpcodeNeedsReply(opcode byte) bool {
	switch opcode {
	case
		3,   // GetWindowAttributes
		14,  // GetGeometry
		15,  // QueryTree
		16,  // InternAtom
		17,  // GetAtomName
		20,  // GetProperty
		23,  // GetSelectionOwner
		26,  // GrabPointer
		28,  // GrabKeyboard
		34,  // GetMotionEvents
		38,  // TranslateCoordinates
		43,  // GetInputFocus
		44,  // QueryKeymap
		47,  // QueryFont
		49,  // QueryTextExtents
		50,  // ListFonts
		51,  // ListFontsWithInfo
		52,  // GetFontPath
		60,  // GetImage
		73,  // QueryBestSize
		98,  // QueryExtension
		99,  // ListExtensions
		101, // GetKeyboardMapping
		103, // GetKeyboardControl
		106, // GetPointerControl
		108, // GetScreenSaver
		110, // ListHosts
		116, // GetPointerMapping
		117: // GetModifierMapping
		return true
	}
	return false
}


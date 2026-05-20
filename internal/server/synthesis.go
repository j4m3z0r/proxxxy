package server

import (
	"encoding/binary"
	"log"

	"james.id.au/proxxxy/internal/wire"
	"james.id.au/proxxxy/internal/x11"
)

// sendX11Setup sends a MsgX11Setup message to the client carrying the
// connection setup request bytes, the resource-id-base/mask from the
// original real X server reply, and the app's current sequence number.
// The client uses these to establish a new X connection with ID remapping
// and to rewrite sequence numbers on forwarded replies.
func (s *Server) sendX11Setup(connID uint32, setupReq []byte, ridBase, ridMask, appSeqNum uint32) {
	p := make([]byte, 4+4+4+4+len(setupReq))
	binary.LittleEndian.PutUint32(p[0:4], connID)
	binary.LittleEndian.PutUint32(p[4:8], ridBase)
	binary.LittleEndian.PutUint32(p[8:12], ridMask)
	binary.LittleEndian.PutUint32(p[12:16], appSeqNum)
	copy(p[16:], setupReq)
	s.WriteToClient(wire.MsgX11Setup, p) //nolint:errcheck
}

// synthesiseAppConn generates the minimum X11 commands needed to reconstruct
// the visible state of ac on a fresh client display connection.
func (s *Server) synthesiseAppConn(ac *x11.AppConn) {
	setupReq := ac.SetupReq()
	if len(setupReq) == 0 {
		return
	}
	ridBase, ridMask := ac.RID()
	if ridBase == 0 {
		// App connected before any proxxxy-client joined and is still blocked
		// waiting for its X11 setup reply. Skip synthesis here; after
		// synthActive is cleared, relayPendingSetups will re-send the setup
		// bytes so the client opens a fresh X connection and the real setup
		// reply reaches the waiting app.
		log.Printf("server: synthesis conn %d: skipping (pending setup reply)", ac.ID)
		return
	}

	// 0. Connection setup — must arrive first so the client can establish
	//    a real X connection before any resource-creation commands follow.
	{
		log.Printf("server: synthesis conn %d: ridBase=0x%08x ridMask=0x%08x setupLen=%d",
			ac.ID, ridBase, ridMask, len(setupReq))
		s.sendX11Setup(ac.ID, setupReq, ridBase, ridMask, ac.SeqNum())
	}

	// 1. Windows: parent-first walk (no mapping yet).
	windows := ac.Windows()
	roots := findRoots(windows)
	for _, root := range roots {
		s.synthesiseWindowCreate(ac.ID, windows, root, ac.Order)
	}

	// 2. Pixmaps: create only (draw commands come after GCs, which they reference).
	pixmaps := ac.Pixmaps()
	for _, pm := range pixmaps {
		if cr := pm.CreateReq(); len(cr) > 0 {
			log.Printf("server: synthesis conn %d: CreatePixmap 0x%08x %dx%d depth=%d len=%d",
				ac.ID, pm.ID, pm.Width, pm.Height, pm.Depth, len(cr))
			s.sendToClient(ac.ID, cr)
		}
	}

	// 2.5. Fonts: open before GCs so GCs that reference a font ID don't fail with BadFont.
	fonts := ac.Fonts()
	for _, f := range fonts {
		if or := f.OpenReq(); len(or) > 0 {
			log.Printf("server: synthesis conn %d: OpenFont 0x%08x %q", ac.ID, f.ID, f.Name)
			s.sendToClient(ac.ID, or)
		}
	}

	// 3. GCs: create + replay all attribute changes.
	// Must come before pixmap draw commands since draws reference GC IDs.
	gcs := ac.GCs()
	for _, gc := range gcs {
		if cr := gc.CreateReq(); len(cr) > 0 {
			cr = sanitizeGCDrawable(cr, gc.Drawable, windows, pixmaps, ac.Order)
			cr = sanitizeGCFont(cr, fonts, ac.Order)
			log.Printf("server: synthesis conn %d: CreateGC 0x%08x drawable=0x%08x len=%d",
				ac.ID, gc.ID, gc.Drawable, len(cr))
			s.sendToClient(ac.ID, cr)
		}
		for _, cmd := range gc.ChangeCmds {
			s.sendToClient(ac.ID, cmd)
		}
	}

	// 4. RENDER Pictures: recreate before pixmap draw commands, since those
	//    may reference picture objects via RENDER draw calls.
	for _, pic := range ac.Pictures() {
		if cr := pic.CreateReq(); len(cr) > 0 {
			log.Printf("server: synthesis conn %d: CreatePicture 0x%08x drawable=0x%08x",
				ac.ID, pic.ID, pic.Drawable)
			s.sendToClient(ac.ID, cr)
			for _, cmd := range pic.ChangeCmds {
				s.sendToClient(ac.ID, cmd)
			}
		}
	}

	// 4b. RENDER GlyphSets: recreate and repopulate before any CompositeGlyphs
	//     calls can arrive via Expose-triggered redraws. GlyphSets use IDs from
	//     the app's resource-id space but are freed when the last referencing
	//     connection closes (i.e. when the client disconnects).
	for _, gs := range ac.GlyphSets() {
		if cr := gs.CreateReq(); len(cr) > 0 {
			log.Printf("server: synthesis conn %d: CreateGlyphSet 0x%08x len=%d addCmds=%d",
				ac.ID, gs.ID, len(cr), len(gs.AddGlyphsCmds))
			s.sendToClient(ac.ID, cr)
			for _, cmd := range gs.AddGlyphsCmds {
				s.sendToClient(ac.ID, cmd)
			}
		}
	}

	// 5. Pixmap draw commands: replay after GCs and Pictures exist.
	// Skip commands that reference a GC that was freed before this reconnect —
	// those GCs are gone from the synthesis xconn and would cause BadGC errors.
	// ClearArea (opcode 61) has no GC field; all other tracked draw opcodes
	// carry the GC at bytes [8:12].
	for _, pm := range pixmaps {
		for _, cmd := range pm.DrawCmds {
			if len(cmd) >= 12 && cmd[0] != x11.OpcodeClearArea {
				gcID := ac.Order.Uint32(cmd[8:12])
				if _, ok := gcs[gcID]; !ok {
					continue
				}
			}
			s.sendToClient(ac.ID, cmd)
		}
	}

	// 6. Map windows.
	for _, root := range roots {
		s.synthesiseWindowMap(ac.ID, windows, root, ac.Order)
	}
}

// synthesiseWindowCreate sends CreateWindow for a window and its subtree
// (without mapping them yet).
func (s *Server) synthesiseWindowCreate(connID uint32, all map[uint32]x11.Window, wid uint32, order binary.ByteOrder) {
	w, ok := all[wid]
	if !ok {
		return
	}
	if cr := w.CreateReq(); len(cr) > 0 {
		cr = sanitizeCreateWindow(cr, order)
		log.Printf("server: synthesis conn %d: CreateWindow 0x%08x parent=0x%08x mapped=%v len=%d",
			connID, w.ID, w.Parent, w.Mapped, len(cr))
		s.sendToClient(connID, cr)
	}
	for _, child := range w.Children {
		s.synthesiseWindowCreate(connID, all, child, order)
	}
}

// synthesiseWindowMap sends MapWindow for all mapped windows in the subtree.
func (s *Server) synthesiseWindowMap(connID uint32, all map[uint32]x11.Window, wid uint32, order binary.ByteOrder) {
	w, ok := all[wid]
	if !ok {
		return
	}
	if w.Mapped {
		s.sendToClient(connID, makeMapWindow(wid, order))
	}
	for _, child := range w.Children {
		s.synthesiseWindowMap(connID, all, child, order)
	}
}

// synthesisExpose injects a fake Expose event directly into the app's X socket
// for each mapped window, causing the app to redraw without the client relay.
func (s *Server) synthesisExpose(ac *x11.AppConn) {
	s.mu.Lock()
	app := s.appConns[ac.ID]
	s.mu.Unlock()
	if app == nil {
		return
	}
	seqNum := ac.SeqNum()
	for _, w := range ac.Windows() {
		if !w.Mapped || w.Width == 0 || w.Height == 0 {
			continue
		}
		log.Printf("server: synthesis conn %d: injecting Expose for 0x%08x %dx%d",
			ac.ID, w.ID, w.Width, w.Height)
		evt := makeExposeEvent(w.ID, w.Width, w.Height, seqNum, ac.Order)
		app.Write(evt) //nolint:errcheck
	}
}

// sanitizeCreateWindow removes the CWColormap attribute from a CreateWindow
// request and resets depth and visual to CopyFromParent (0). This prevents
// synthesis from failing when the original colormap was created by a
// now-dead X connection: without CWColormap, X inherits the parent's colormap,
// and without an explicit visual/depth, X inherits those too (always valid for
// children of the root window).
func sanitizeCreateWindow(req []byte, order binary.ByteOrder) []byte {
	if len(req) < 32 || req[0] != x11.OpcodeCreateWindow {
		return req
	}
	valueMask := order.Uint32(req[28:32])
	if valueMask&(1<<13) == 0 {
		// No CWColormap — nothing to strip.
		return req
	}

	// Locate the colormap value in the value-list (values ordered by bit position).
	off := 32
	for bit := uint(0); bit < 13; bit++ {
		if valueMask&(1<<bit) != 0 {
			off += 4
		}
	}
	if len(req) < off+4 {
		return req
	}

	// Build a new request omitting the 4-byte colormap value.
	out := make([]byte, len(req)-4)
	copy(out, req[:off])
	copy(out[off:], req[off+4:])

	order.PutUint32(out[28:32], valueMask&^uint32(1<<13)) // clear CWColormap
	out[1] = 0                                             // depth = CopyFromParent
	order.PutUint32(out[24:28], 0)                         // visual = CopyFromParent
	order.PutUint16(out[2:4], uint16(len(out)/4))          // recompute length
	return out
}

// sanitizeGCDrawable rewrites a CreateGC request to use a fallback drawable if
// the original drawable no longer exists (e.g., a pixmap that was freed before
// reconnect). The GC's screen affinity comes from its drawable; substituting any
// valid window on the same screen keeps the GC usable for drawing.
// When substituting, GCTile (bit 10) is also stripped: a tile pixmap's depth
// must match the drawable's depth, and we cannot guarantee that after substitution.
func sanitizeGCDrawable(req []byte, drawable uint32, windows map[uint32]x11.Window, pixmaps map[uint32]x11.Pixmap, order binary.ByteOrder) []byte {
	if len(req) < 16 || req[0] != x11.OpcodeCreateGC {
		return req
	}
	if _, ok := windows[drawable]; ok {
		return req
	}
	if _, ok := pixmaps[drawable]; ok {
		return req
	}
	// Drawable is gone — substitute with the first available window.
	var fallbackID uint32
	for id := range windows {
		fallbackID = id
		break
	}
	if fallbackID == 0 {
		return req // no windows either (shouldn't happen in practice)
	}
	out := make([]byte, len(req))
	copy(out, req)
	order.PutUint32(out[8:12], fallbackID)

	// Strip GCTile (bit 10): tile depth must equal drawable depth, which we
	// cannot guarantee after substituting an arbitrary window.
	const gcTileBit = uint32(1 << 10)
	valueMask := order.Uint32(out[12:16])
	if valueMask&gcTileBit == 0 {
		return out
	}
	tileOff := 16
	for bit := uint(0); bit < 10; bit++ {
		if valueMask&(1<<bit) != 0 {
			tileOff += 4
		}
	}
	if len(out) < tileOff+4 {
		return out
	}
	result := make([]byte, len(out)-4)
	copy(result, out[:tileOff])
	copy(result[tileOff:], out[tileOff+4:])
	order.PutUint32(result[12:16], valueMask&^gcTileBit)
	order.PutUint16(result[2:4], uint16(len(result)/4))
	return result
}

// sanitizeGCFont strips GCFont (bit 14) from a CreateGC request if the
// referenced font is not in the tracked font set. Without this, CreateGC fails
// with BadFont when a font was closed before reconnect, which then causes
// BadGC for every subsequent draw command using that GC.
func sanitizeGCFont(req []byte, fonts map[uint32]x11.Font, order binary.ByteOrder) []byte {
	if len(req) < 16 || req[0] != x11.OpcodeCreateGC {
		return req
	}
	const gcFontBit = uint32(1 << 14)
	valueMask := order.Uint32(req[12:16])
	if valueMask&gcFontBit == 0 {
		return req // no GCFont — nothing to strip
	}
	// Locate GCFont in the value-list (values ordered by bit position).
	fontOff := 16
	for bit := uint(0); bit < 14; bit++ {
		if valueMask&(1<<bit) != 0 {
			fontOff += 4
		}
	}
	if len(req) < fontOff+4 {
		return req
	}
	fontID := order.Uint32(req[fontOff:])
	if _, ok := fonts[fontID]; ok {
		return req // font is open — keep GCFont as-is
	}
	// Font not tracked: strip the GCFont 4-byte value from the list.
	out := make([]byte, len(req)-4)
	copy(out, req[:fontOff])
	copy(out[fontOff:], req[fontOff+4:])
	order.PutUint32(out[12:16], valueMask&^gcFontBit)
	order.PutUint16(out[2:4], uint16(len(out)/4))
	return out
}

func findRoots(windows map[uint32]x11.Window) []uint32 {
	var roots []uint32
	for id, w := range windows {
		if _, hasParent := windows[w.Parent]; !hasParent {
			roots = append(roots, id)
		}
	}
	return roots
}

func makeMapWindow(wid uint32, order binary.ByteOrder) []byte {
	req := make([]byte, 8)
	req[0] = x11.OpcodeMapWindow
	order.PutUint16(req[2:], 2)
	order.PutUint32(req[4:], wid)
	return req
}

func makeExposeEvent(wid uint32, width, height uint16, seqNum uint32, order binary.ByteOrder) []byte {
	evt := make([]byte, 32)
	evt[0] = 12 // Expose event code
	// evt[1]: unused
	order.PutUint16(evt[2:], uint16(seqNum))
	order.PutUint32(evt[4:], wid)
	// x, y = 0,0
	order.PutUint16(evt[12:], width)
	order.PutUint16(evt[14:], height)
	// count = 0 (last in series)
	return evt
}

// runSynthesis sends SESSION_RESUME, synthesised state, then SESSION_LIVE.
func (s *Server) runSynthesis() {
	s.WriteToClient(wire.MsgSessionResume, nil)

	s.mu.Lock()
	states := make([]*x11.AppConn, 0, len(s.appState))
	for _, ac := range s.appState {
		states = append(states, ac)
	}
	s.mu.Unlock()

	for _, ac := range states {
		wins := ac.Windows()
		gcs := ac.GCs()
		pms := ac.Pixmaps()
		log.Printf("server: synthesising state for conn %d: %d windows %d GCs %d pixmaps",
			ac.ID, len(wins), len(gcs), len(pms))
		s.synthesiseAppConn(ac)
	}

	// Send SESSION_LIVE before injecting Expose events. This ensures xterm's
	// redraw responses reach the client after it has entered live-relay mode,
	// so replies from the synthesis X connection are forwarded (not drained).
	s.WriteToClient(wire.MsgSessionLive, nil)

	// Clear live-relay block BEFORE injecting Expose. Draw commands triggered by
	// the app's Expose handler must not be discarded by sendMsgsToClient.
	s.mu.Lock()
	s.synthActive = false
	s.mu.Unlock()

	for _, ac := range states {
		s.synthesisExpose(ac)
	}
}

package server

import (
	"encoding/binary"
	"log"

	"james.id.au/proxxxy/internal/wire"
	"james.id.au/proxxxy/internal/x11"
)

// sendX11Setup sends a MsgX11Setup message to the client carrying the
// connection setup request bytes and the resource-id-base/mask from the
// original real X server reply. The client uses these to establish a new
// X connection with ID remapping.
func (s *Server) sendX11Setup(connID uint32, setupReq []byte, ridBase, ridMask uint32) {
	p := make([]byte, 4+4+4+len(setupReq))
	binary.LittleEndian.PutUint32(p[0:4], connID)
	binary.LittleEndian.PutUint32(p[4:8], ridBase)
	binary.LittleEndian.PutUint32(p[8:12], ridMask)
	copy(p[12:], setupReq)
	s.WriteToClient(wire.MsgX11Setup, p) //nolint:errcheck
}

// synthesiseAppConn generates the minimum X11 commands needed to reconstruct
// the visible state of ac on a fresh client display connection.
func (s *Server) synthesiseAppConn(ac *x11.AppConn) {
	// 0. Connection setup — must arrive first so the client can establish
	//    a real X connection before any resource-creation commands follow.
	if setupReq := ac.SetupReq(); len(setupReq) > 0 {
		ridBase, ridMask := ac.RID()
		log.Printf("server: synthesis conn %d: ridBase=0x%08x ridMask=0x%08x setupLen=%d",
			ac.ID, ridBase, ridMask, len(setupReq))
		s.sendX11Setup(ac.ID, setupReq, ridBase, ridMask)
	}

	// 1. Windows: parent-first walk (no mapping yet).
	windows := ac.Windows()
	roots := findRoots(windows)
	for _, root := range roots {
		s.synthesiseWindowCreate(ac.ID, windows, root)
	}

	// 2. Pixmaps: create only (draw commands come after GCs, which they reference).
	pixmaps := ac.Pixmaps()
	for _, pm := range pixmaps {
		if cr := pm.CreateReq(); len(cr) > 0 {
			s.sendToClient(ac.ID, cr)
		}
	}

	// 3. GCs: create + replay all attribute changes.
	// Must come before pixmap draw commands since draws reference GC IDs.
	for _, gc := range ac.GCs() {
		if cr := gc.CreateReq(); len(cr) > 0 {
			s.sendToClient(ac.ID, cr)
		}
		for _, cmd := range gc.ChangeCmds {
			s.sendToClient(ac.ID, cmd)
		}
	}

	// 4. Pixmap draw commands: replay after GCs exist.
	for _, pm := range pixmaps {
		for _, cmd := range pm.DrawCmds {
			s.sendToClient(ac.ID, cmd)
		}
	}

	// 5. Map windows.
	for _, root := range roots {
		s.synthesiseWindowMap(ac.ID, windows, root, ac.Order)
	}
}

// synthesiseWindowCreate sends CreateWindow for a window and its subtree
// (without mapping them yet).
func (s *Server) synthesiseWindowCreate(connID uint32, all map[uint32]x11.Window, wid uint32) {
	w, ok := all[wid]
	if !ok {
		return
	}
	if cr := w.CreateReq(); len(cr) > 0 {
		log.Printf("server: synthesis conn %d: CreateWindow 0x%08x parent=0x%08x mapped=%v len=%d",
			connID, w.ID, w.Parent, w.Mapped, len(cr))
		s.sendToClient(connID, cr)
	}
	for _, child := range w.Children {
		s.synthesiseWindowCreate(connID, all, child)
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

	for _, ac := range states {
		s.synthesisExpose(ac)
	}
}

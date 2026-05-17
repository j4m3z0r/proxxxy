package server

import (
	"encoding/binary"
	"log"

	"james.id.au/proxxxy/internal/wire"
	"james.id.au/proxxxy/internal/x11"
)

// synthesiseAppConn generates the minimum X11 commands needed to reconstruct
// the visible state of ac on a fresh client display connection.
func (s *Server) synthesiseAppConn(ac *x11.AppConn) {
	// 1. Pixmaps: create + replay draw commands.
	for _, pm := range ac.Pixmaps() {
		if cr := pm.CreateReq(); len(cr) > 0 {
			s.sendToClient(ac.ID, cr)
		}
		for _, cmd := range pm.DrawCmds {
			s.sendToClient(ac.ID, cmd)
		}
	}

	// 2. GCs.
	for _, gc := range ac.GCs() {
		if cr := gc.CreateReq(); len(cr) > 0 {
			s.sendToClient(ac.ID, cr)
		}
	}

	// 3. Windows: parent-first walk.
	windows := ac.Windows()
	roots := findRoots(windows)
	for _, root := range roots {
		s.synthesiseWindowTree(ac.ID, windows, root, ac.Order)
	}
}

func (s *Server) synthesiseWindowTree(connID uint32, all map[uint32]x11.Window, wid uint32, order binary.ByteOrder) {
	w, ok := all[wid]
	if !ok {
		return
	}
	if cr := w.CreateReq(); len(cr) > 0 {
		s.sendToClient(connID, cr)
	}
	if w.Mapped {
		s.sendToClient(connID, makeMapWindow(wid, order))
	}
	for _, child := range w.Children {
		s.synthesiseWindowTree(connID, all, child, order)
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
		log.Printf("server: synthesising state for conn %d", ac.ID)
		s.synthesiseAppConn(ac)
	}

	s.WriteToClient(wire.MsgSessionLive, nil)
}

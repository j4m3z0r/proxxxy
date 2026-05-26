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

	// 0.5. MIT-SHM segments: re-attach before any draw commands that may use
	//      ShmPutImage. The shmid in each ShmAttach is still valid (GIMP is alive),
	//      so the real X server can attach the shared memory to this new connection.
	for seg, req := range ac.ShmSegs() {
		log.Printf("server: synthesis conn %d: ShmAttach 0x%08x", ac.ID, seg)
		s.sendToClient(ac.ID, req)
	}

	// 0.6. SYNC counters: recreate before live relay starts. GTK windows use
	//      SYNC counters for the _NET_WM_SYNC_REQUEST protocol. When the WM sends
	//      a sync request, GTK calls SetCounter on the counter. If the counter does
	//      not exist (freed when the old connection died), SetCounter returns
	//      XSyncBadCounter, causing GDK's error handler to abort the process.
	for ctr, req := range ac.SyncCounters() {
		log.Printf("server: synthesis conn %d: SyncCreateCounter 0x%08x", ac.ID, ctr)
		s.sendToClient(ac.ID, req)
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

	// 2.1. SHM pixmaps: replay the original ShmCreatePixmap requests.
	//      The shmseg IDs were reattached in step 3; the drawable IDs were
	//      synthesised in steps 5-6. Sending the raw request preserves the
	//      SHM-backed property that some apps (e.g. Firefox's SWGL renderer)
	//      need in order to map the framebuffer directly via the kernel SHM
	//      segment. The pixmaps are already tracked in the pixmaps map (via
	//      AppConn.Pixmaps, which includes SHM pixmaps from state.go).
	{
		shmPMs := ac.ShmPixmaps()
		for pid, req := range shmPMs {
			if len(req) < 28 {
				continue
			}
			depth := req[16]
			width := ac.Order.Uint16(req[12:14])
			height := ac.Order.Uint16(req[14:16])
			shmseg := ac.Order.Uint32(req[20:24])
			log.Printf("server: synthesis conn %d: ShmCreatePixmap 0x%08x %dx%d depth=%d shmseg=0x%08x",
				ac.ID, pid, width, height, depth, shmseg)
			s.sendToClient(ac.ID, req)
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

	// 2.6. Core cursors: create after pixmaps and fonts (CreateCursor uses pixmaps;
	// CreateGlyphCursor uses fonts). RenderCreateCursor cursors are deferred to
	// step 4c after RENDER Pictures, since they reference Picture IDs.
	for _, cur := range ac.Cursors() {
		cr := cur.CreateReq()
		if len(cr) < 12 {
			continue
		}
		if cr[0] == x11.OpcodeRender {
			continue // RENDER cursor — handled in step 4c
		}
		if len(cr) < 16 {
			continue
		}
		switch cr[0] {
		case x11.OpcodeCreateCursor:
			srcPM := ac.Order.Uint32(cr[8:12])
			if _, ok := pixmaps[srcPM]; !ok {
				log.Printf("server: synthesis conn %d: skip CreateCursor 0x%08x (source pixmap 0x%08x gone)",
					ac.ID, cur.ID, srcPM)
				continue
			}
		case x11.OpcodeCreateGlyphCursor:
			srcFont := ac.Order.Uint32(cr[8:12])
			if _, ok := fonts[srcFont]; !ok {
				fontName := cur.SrcFontName()
				if fontName == "" {
					log.Printf("server: synthesis conn %d: skip CreateGlyphCursor 0x%08x (font 0x%08x gone, name unknown)",
						ac.ID, cur.ID, srcFont)
					continue
				}
				// Font was closed after cursor creation. Reopen it with the same
				// ID so CreateGlyphCursor succeeds, then close it — the cursor
				// outlives the font, matching original session behaviour.
				log.Printf("server: synthesis conn %d: reopen font %q (0x%08x) for cursor 0x%08x",
					ac.ID, fontName, srcFont, cur.ID)
				s.sendToClient(ac.ID, makeOpenFont(srcFont, fontName, ac.Order))
				log.Printf("server: synthesis conn %d: CreateGlyphCursor 0x%08x", ac.ID, cur.ID)
				s.sendToClient(ac.ID, cr)
				s.sendToClient(ac.ID, makeCloseFont(srcFont, ac.Order))
				continue
			}
		}
		log.Printf("server: synthesis conn %d: CreateCursor 0x%08x opcode=%d", ac.ID, cur.ID, cr[0])
		s.sendToClient(ac.ID, cr)
	}

	// 3. GCs: create + replay all attribute changes.
	// Must come before pixmap draw commands since draws reference GC IDs.
	//
	// scratchPixmaps: depth → pixmap ID. When a GC's original drawable is
	// gone and no depth-matched drawable exists, we create a 1×1 scratch
	// pixmap so the GC gets the correct depth. IDs count down from the top
	// of the resource range to avoid colliding with GIMP's upward-counting IDs.
	scratchPixmaps := make(map[byte]uint32)
	// Start scratch IDs at ridBase|(ridMask - ridMask/4) and count DOWN.
	// The top quarter of the range (ridMask - ridMask/4 + 1 .. ridMask) is
	// reserved for the client-side scratch colormaps (injectColormap in client.go),
	// which also allocate downward from ridBase|ridMask. The lower three quarters
	// of the range belong to GIMP resources and synthesis scratch pixmaps.
	scratchCounter := ridMask - ridMask/4 // counts down
	ensureScratchPixmap := func(depth byte, drawableWin uint32) uint32 {
		if id, ok := scratchPixmaps[depth]; ok {
			return id
		}
		id := ridBase | scratchCounter
		scratchCounter--
		cr := make([]byte, 16)
		cr[0] = x11.OpcodeCreatePixmap
		cr[1] = depth
		ac.Order.PutUint16(cr[2:4], 4)
		ac.Order.PutUint32(cr[4:8], id)
		ac.Order.PutUint32(cr[8:12], drawableWin)
		ac.Order.PutUint16(cr[12:14], 1) // width=1
		ac.Order.PutUint16(cr[14:16], 1) // height=1
		log.Printf("server: synthesis conn %d: CreatePixmap(scratch) 0x%08x depth=%d",
			ac.ID, id, depth)
		s.sendToClient(ac.ID, cr)
		// Register in pixmaps map so sanitizeGCDrawable finds it.
		pixmaps[id] = x11.Pixmap{ID: id, Depth: depth, Width: 1, Height: 1}
		scratchPixmaps[depth] = id
		return id
	}

	// Pick any surviving window as the drawable for scratch pixmap creation.
	var anyWin uint32
	for id := range windows {
		anyWin = id
		break
	}

	gcs := ac.GCs()
	for _, gc := range gcs {
		if cr := gc.CreateReq(); len(cr) > 0 {
			// If the original drawable is gone and no depth-matched drawable
			// exists yet, create a scratch pixmap of the required depth first.
			// This preserves the GC's depth property across synthesis, preventing
			// BadMatch when the app uses the GC on a drawable of the original depth.
			if gc.DrawableDepth != 0 && anyWin != 0 {
				_, inWin := windows[gc.Drawable]
				_, inPM := pixmaps[gc.Drawable]
				if !inWin && !inPM {
					// Drawable is gone. Check if any depth-matched drawable exists.
					needScratch := true
					for _, w := range windows {
						if w.Depth == gc.DrawableDepth {
							needScratch = false
							break
						}
					}
					if needScratch {
						for _, pm := range pixmaps {
							if pm.Depth == gc.DrawableDepth {
								needScratch = false
								break
							}
						}
					}
					if needScratch {
						ensureScratchPixmap(gc.DrawableDepth, anyWin)
					}
				}
			}
			cr = sanitizeGCDrawable(cr, gc.Drawable, gc.DrawableDepth, windows, pixmaps, ac.Order)
			cr = sanitizeGCPixmapAttrs(cr, pixmaps, ac.Order)
			cr = sanitizeGCFont(cr, fonts, ac.Order)
			logDrawable := gc.Drawable
			if ac.Order.Uint32(cr[8:12]) != gc.Drawable {
				logDrawable = ac.Order.Uint32(cr[8:12]) // substituted
			}
			log.Printf("server: synthesis conn %d: CreateGC 0x%08x drawable=0x%08x len=%d",
				ac.ID, gc.ID, logDrawable, len(cr))
			s.sendToClient(ac.ID, cr)
		}
		for _, cmd := range gc.ChangeCmds {
			s.sendToClient(ac.ID, cmd)
		}
	}

	// 4. RENDER Pictures: recreate before pixmap draw commands, since those
	//    may reference picture objects via RENDER draw calls.
	pictures := ac.Pictures()
	for _, pic := range pictures {
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

	// 4c. RENDER cursors: create after Pictures (RenderCreateCursor references a
	//     source Picture; the cursor ID is in the core cursor namespace and freed
	//     with the core FreeCursor request).
	// Firefox creates a pixmap-backed picture, creates the cursor from it, then
	// frees both immediately. By synthesis time the pixmap is gone, so recreating
	// the picture fails with BadDrawable. In that case we fall back to a glyph
	// cursor from the standard "cursor" font so the cursor ID exists in the
	// synthesis xconn (avoiding BadCursor in live traffic). The cursor won't look
	// right but won't crash.
	for _, cur := range ac.Cursors() {
		cr := cur.CreateReq()
		if len(cr) < 12 || cr[0] != x11.OpcodeRender || cr[1] != x11.RenderCreateCursor {
			continue
		}
		srcPic := ac.Order.Uint32(cr[8:12])
		if _, ok := pictures[srcPic]; ok {
			log.Printf("server: synthesis conn %d: RenderCreateCursor 0x%08x src_picture=0x%08x",
				ac.ID, cur.ID, srcPic)
			s.sendToClient(ac.ID, cr)
			continue
		}
		// Source picture absent. Try recreating it from the saved create request.
		// This works if the picture was a solid-fill/gradient (no backing drawable).
		// If the picture was backed by a pixmap that is now freed, fall back to a
		// glyph cursor.
		picReq := cur.RenderPicReq()
		if len(picReq) >= 12 && picReq[1] == x11.RenderCreatePicture {
			// Check that the backing drawable is still alive.
			drawable := ac.Order.Uint32(picReq[8:12])
			_, pmOK := pixmaps[drawable]
			_, winOK := windows[drawable]
			if !pmOK && !winOK {
				// Drawable gone — use glyph cursor fallback.
				tmpFontID := ridBase | scratchCounter
				scratchCounter--
				log.Printf("server: synthesis conn %d: RenderCreateCursor 0x%08x fallback glyph cursor (drawable 0x%08x gone)",
					ac.ID, cur.ID, drawable)
				s.sendToClient(ac.ID, makeOpenFont(tmpFontID, "cursor", ac.Order))
				s.sendToClient(ac.ID, makeCreateGlyphCursor(cur.ID, tmpFontID, ac.Order))
				s.sendToClient(ac.ID, makeCloseFont(tmpFontID, ac.Order))
				continue
			}
		}
		if len(picReq) == 0 {
			log.Printf("server: synthesis conn %d: skip RenderCreateCursor 0x%08x (source picture 0x%08x gone, no saved req)",
				ac.ID, cur.ID, srcPic)
			continue
		}
		log.Printf("server: synthesis conn %d: recreate source picture 0x%08x for cursor 0x%08x",
			ac.ID, srcPic, cur.ID)
		s.sendToClient(ac.ID, picReq)
		log.Printf("server: synthesis conn %d: RenderCreateCursor 0x%08x src_picture=0x%08x",
			ac.ID, cur.ID, srcPic)
		s.sendToClient(ac.ID, cr)
		s.sendToClient(ac.ID, makeFreePicture(srcPic, ac.Order))
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

// synthesiseWindowCreate sends CreateWindow (and ChangeWindowAttributes for
// input masks) for a window and its subtree, without mapping them yet.
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
		// Replay the app's XSelectInput event mask. GTK3 issues
		// ChangeWindowAttributes with CWEventMask after CreateWindow to add
		// ButtonPressMask, KeyPressMask, etc. Without this, the synthesis
		// xconn window receives no input events from the real X server, so
		// clicks and keystrokes never reach GIMP after reconnect.
		if w.EventMask != 0 {
			log.Printf("server: synthesis conn %d: SelectInput 0x%08x mask=0x%08x",
				connID, w.ID, w.EventMask)
			s.sendToClient(connID, makeSelectInput(wid, w.EventMask, order))
		}
		// Replay XISelectEvents (XI2 input masks). GTK3 uses XISelectEvents
		// (not XSelectInput) for button/key events — without replay, clicks
		// and keypresses are not delivered after reconnect.
		for _, xi2req := range w.Xi2Masks {
			log.Printf("server: synthesis conn %d: XISelectEvents 0x%08x len=%d",
				connID, w.ID, len(xi2req))
			s.sendToClient(connID, xi2req)
		}
		for _, pcmd := range w.PropertyCmds {
			s.sendToClient(connID, pcmd)
		}
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

// synthesisConfigureNotify injects fake ConfigureNotify events for each mapped
// window. GTK3/GDK tracks a configure_request_count that it increments when it
// sends ConfigureWindow and decrements on ConfigureNotify. During the client
// outage, ConfigureWindow requests from the app were discarded by proxxxy, but
// configure_request_count was still incremented by N (the number of
// ConfigureWindow requests sent). Without N matching ConfigureNotify events,
// the count stays > 0, permanently blocking gdk_window_process_updates_with_mode.
// pendingCfgs is the per-window count from DrainPendingConfigures; we inject
// max(1, pendingCfgs[wid]) events so the count reaches zero regardless of N.
func (s *Server) synthesisConfigureNotify(ac *x11.AppConn, pendingCfgs map[uint32]uint32) {
	s.mu.Lock()
	app := s.appConns[ac.ID]
	s.mu.Unlock()
	if app == nil {
		return
	}
	seqNum := ac.SeqNum()
	windows := ac.Windows()
	for _, w := range windows {
		if !w.Mapped || w.Width == 0 || w.Height == 0 {
			continue
		}
		// Only inject for toplevel windows (parent is not another app window).
		// configure_request_count is tracked by GTK3 only for WM-managed toplevels.
		// Injecting ConfigureNotify for child windows corrupts GTK3's layout state.
		if _, parentIsApp := windows[w.Parent]; parentIsApp {
			continue
		}
		count := pendingCfgs[w.ID]
		if count < 1 {
			count = 1
		}
		log.Printf("server: synthesis conn %d: injecting %d ConfigureNotify(s) for 0x%08x %dx%d",
			ac.ID, count, w.ID, w.Width, w.Height)
		for i := uint32(0); i < count; i++ {
			evt := makeConfigureNotifyEvent(w.ID, w.X, w.Y, w.Width, w.Height, w.BorderWidth, seqNum, ac.Order)
			app.Write(evt) //nolint:errcheck
		}
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

// synthesisInjectShmCompletions sends a fake ShmCompletion event for every
// SHM segment currently attached by the app connection. This unblocks an SWGL
// compositor that is stuck waiting for a ShmCompletion that the previous
// proxxxy-client dropped when it was killed.
func (s *Server) synthesisInjectShmCompletions(ac *x11.AppConn) {
	s.mu.Lock()
	app := s.appConns[ac.ID]
	s.mu.Unlock()
	if app == nil {
		return
	}
	segs := ac.ShmSegs()
	if len(segs) == 0 {
		return
	}
	seqNum := uint16(ac.SeqNum())
	for seg := range segs {
		log.Printf("server: synthesis conn %d: injecting ShmCompletion shmseg=0x%08x",
			ac.ID, seg)
		sendFakeShmCompletion(app, seqNum, 0, seg, 0, ac.Order)
	}
}

// sanitizeCreateWindow removes the CWColormap and CWCursor attributes from a
// CreateWindow request. CWColormap removal prevents BadColor when the original
// colormap was created by a now-dead connection; CWCursor removal prevents
// BadCursor because cursors are synthesized in step 2.6, after windows. Depth
// and visual are reset to CopyFromParent (0) alongside CWColormap removal.
func sanitizeCreateWindow(req []byte, order binary.ByteOrder) []byte {
	if len(req) < 32 || req[0] != x11.OpcodeCreateWindow {
		return req
	}
	// Strip CWColormap (bit 13): the original colormap was created by the app's
	// X connection which is now dead; keeping it would cause BadColor on the
	// new display. Depth and visual are intentionally preserved so that
	// depth-32 ARGB windows keep working on the new display connection (both
	// displays are xorg-dummy with matching visual IDs, so the original
	// depth/visual values are valid on the synthesis xconn).
	req = stripCreateWindowAttr(req, 13, order)
	// Strip CWCursor (bit 14): cursors are not yet synthesized at this point.
	req = stripCreateWindowAttr(req, 14, order)
	return req
}

// stripCreateWindowAttr removes the value for a single attribute bit from a
// CreateWindow request's value list and clears that bit in the value mask.
func stripCreateWindowAttr(req []byte, bit uint, order binary.ByteOrder) []byte {
	if len(req) < 32 {
		return req
	}
	valueMask := order.Uint32(req[28:32])
	if valueMask&(1<<bit) == 0 {
		return req
	}
	off := 32
	for b := uint(0); b < bit; b++ {
		if valueMask&(1<<b) != 0 {
			off += 4
		}
	}
	if len(req) < off+4 {
		return req
	}
	out := make([]byte, len(req)-4)
	copy(out, req[:off])
	copy(out[off:], req[off+4:])
	order.PutUint32(out[28:32], valueMask&^(1<<bit))
	order.PutUint16(out[2:4], uint16(len(out)/4))
	return out
}

// sanitizeGCDrawable rewrites a CreateGC request to use a fallback drawable if
// the original drawable no longer exists (e.g., a pixmap that was freed before
// reconnect). The GC's screen affinity comes from its drawable; substituting any
// valid window on the same screen keeps the GC usable for drawing.
// When substituting, GCTile (bit 10) is also stripped: a tile pixmap's depth
// must match the drawable's depth, and we cannot guarantee that after substitution.
func sanitizeGCDrawable(req []byte, drawable uint32, drawableDepth byte, windows map[uint32]x11.Window, pixmaps map[uint32]x11.Pixmap, order binary.ByteOrder) []byte {
	if len(req) < 16 || req[0] != x11.OpcodeCreateGC {
		return req
	}
	if _, ok := windows[drawable]; ok {
		return req
	}
	if _, ok := pixmaps[drawable]; ok {
		return req
	}
	// Drawable is gone — substitute with a drawable of matching depth so that
	// the GC can still be used without BadMatch errors. Check windows (InputOutput
	// only) first, then pixmaps (which may carry a non-standard depth like 32-bit
	// ARGB that no window on the display supports).
	const classInputOnly = 2
	var fallbackID uint32
	for id, w := range windows {
		if w.Class == classInputOnly {
			continue
		}
		if drawableDepth == 0 || w.Depth == drawableDepth || w.Depth == 0 {
			fallbackID = id
			if drawableDepth == 0 || w.Depth == drawableDepth {
				break
			}
		}
	}
	if fallbackID == 0 && drawableDepth != 0 {
		// No depth-matched window; try surviving pixmaps (e.g. depth-32 ARGB
		// pixmaps that have no matching-depth window on the display).
		for id, pm := range pixmaps {
			if pm.Depth == drawableDepth {
				fallbackID = id
				break
			}
		}
	}
	if fallbackID == 0 {
		// No depth-matched drawable; fall back to any non-InputOnly window.
		for id, w := range windows {
			if w.Class != classInputOnly {
				fallbackID = id
				break
			}
		}
	}
	if fallbackID == 0 {
		// Last resort: any window (e.g. all are InputOnly, which itself would fail).
		for id := range windows {
			fallbackID = id
			break
		}
	}
	if fallbackID == 0 {
		return req // no windows at all (shouldn't happen in practice)
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

// sanitizeGCPixmapAttrs strips GCTile (bit 10) and GCStipple (bit 11) from a
// CreateGC request when the referenced pixmap is not in the tracked set. Freed
// pixmaps would cause BadPixmap on the synthesis X connection.
func sanitizeGCPixmapAttrs(req []byte, pixmaps map[uint32]x11.Pixmap, order binary.ByteOrder) []byte {
	if len(req) < 16 || req[0] != x11.OpcodeCreateGC {
		return req
	}
	for _, bit := range [2]uint{10, 11} { // GCTile, GCStipple
		if len(req) < 16 {
			break
		}
		valueMask := order.Uint32(req[12:16])
		if valueMask&(1<<bit) == 0 {
			continue
		}
		off := 16
		for b := uint(0); b < bit; b++ {
			if valueMask&(1<<b) != 0 {
				off += 4
			}
		}
		if len(req) < off+4 {
			continue
		}
		pmID := order.Uint32(req[off:])
		if _, ok := pixmaps[pmID]; ok {
			continue
		}
		log.Printf("server: synthesis: strip GC bit %d (pixmap 0x%08x gone)", bit, pmID)
		req = stripGCAttr(req, bit, order)
	}
	return req
}

// stripGCAttr removes the value for a single value-list attribute bit from a
// CreateGC request and clears that bit in the value mask.
func stripGCAttr(req []byte, bit uint, order binary.ByteOrder) []byte {
	if len(req) < 16 {
		return req
	}
	valueMask := order.Uint32(req[12:16])
	if valueMask&(1<<bit) == 0 {
		return req
	}
	off := 16
	for b := uint(0); b < bit; b++ {
		if valueMask&(1<<b) != 0 {
			off += 4
		}
	}
	if len(req) < off+4 {
		return req
	}
	out := make([]byte, len(req)-4)
	copy(out, req[:off])
	copy(out[off:], req[off+4:])
	order.PutUint32(out[12:16], valueMask&^(1<<bit))
	order.PutUint16(out[2:4], uint16(len(out)/4))
	return out
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

// makeSelectInput builds a raw ChangeWindowAttributes request that sets only
// the CWEventMask attribute (bit 11) to the given eventMask value.
func makeSelectInput(wid uint32, eventMask uint32, order binary.ByteOrder) []byte {
	req := make([]byte, 16)
	req[0] = x11.OpcodeChangeWindowAttributes
	// byte 1: unused pad
	order.PutUint16(req[2:4], 4) // 4 words = 16 bytes
	order.PutUint32(req[4:8], wid)
	order.PutUint32(req[8:12], 1<<11) // CWEventMask only
	order.PutUint32(req[12:16], eventMask)
	return req
}

func makeMapWindow(wid uint32, order binary.ByteOrder) []byte {
	req := make([]byte, 8)
	req[0] = x11.OpcodeMapWindow
	order.PutUint16(req[2:], 2)
	order.PutUint32(req[4:], wid)
	return req
}

// makeOpenFont builds a raw X11 OpenFont request for the given font ID and name.
func makeOpenFont(fid uint32, name string, order binary.ByteOrder) []byte {
	nameLen := len(name)
	padLen := (4 - nameLen%4) % 4
	totalLen := 12 + nameLen + padLen
	req := make([]byte, totalLen)
	req[0] = x11.OpcodeOpenFont
	order.PutUint16(req[2:], uint16(totalLen/4))
	order.PutUint32(req[4:], fid)
	order.PutUint16(req[8:], uint16(nameLen))
	copy(req[12:], name)
	return req
}

// makeCreateGlyphCursor builds a CreateGlyphCursor request that creates a
// fallback arrow cursor (XC_arrow = char 2 from the "cursor" font). Used when
// the original RENDER cursor's source pixmap is gone and we need any valid
// cursor at the correct resource ID to prevent BadCursor errors in live traffic.
// Layout: [94:1][pad:1][len:2][cid:4][src-font:4][mask-font:4][src-char:2][mask-char:2]
//         [fore-r:2][fore-g:2][fore-b:2][back-r:2][back-g:2][back-b:2] = 32 bytes
func makeCreateGlyphCursor(cid, fontID uint32, order binary.ByteOrder) []byte {
	req := make([]byte, 32)
	req[0] = x11.OpcodeCreateGlyphCursor
	order.PutUint16(req[2:4], 8) // 8 * 4 = 32 bytes
	order.PutUint32(req[4:8], cid)
	order.PutUint32(req[8:12], fontID)  // src-font
	order.PutUint32(req[12:16], fontID) // mask-font
	order.PutUint16(req[16:18], 2)      // XC_arrow source char
	order.PutUint16(req[18:20], 3)      // XC_arrow mask char
	// foreground black: [20:26] = 0
	order.PutUint16(req[26:28], 65535) // back-red
	order.PutUint16(req[28:30], 65535) // back-green
	order.PutUint16(req[30:32], 65535) // back-blue
	return req
}

// makeFreePicture builds a raw RENDER FreePicture request for the given picture ID.
func makeFreePicture(pid uint32, order binary.ByteOrder) []byte {
	req := make([]byte, 8)
	req[0] = x11.OpcodeRender
	req[1] = x11.RenderFreePicture
	order.PutUint16(req[2:], 2)
	order.PutUint32(req[4:], pid)
	return req
}

// makeCloseFont builds a raw X11 CloseFont request for the given font ID.
func makeCloseFont(fid uint32, order binary.ByteOrder) []byte {
	req := make([]byte, 8)
	req[0] = x11.OpcodeCloseFont
	order.PutUint16(req[2:], 2)
	order.PutUint32(req[4:], fid)
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

// makeConfigureNotifyEvent builds a synthetic ConfigureNotify event (code 22).
// X11 ConfigureNotify layout (32 bytes):
//
//	[0]   event code = 22
//	[1]   unused
//	[2:4] sequence number
//	[4:8] event window (same as window for non-substructure-redirect)
//	[8:12] window
//	[12:16] above-sibling = 0 (None)
//	[16:18] x
//	[18:20] y
//	[20:22] width
//	[22:24] height
//	[24:26] border-width
//	[26]  override-redirect = 0
func makeConfigureNotifyEvent(wid uint32, x, y int16, width, height, borderWidth uint16, seqNum uint32, order binary.ByteOrder) []byte {
	evt := make([]byte, 32)
	evt[0] = 22 // ConfigureNotify
	order.PutUint16(evt[2:], uint16(seqNum))
	order.PutUint32(evt[4:], wid)  // event window
	order.PutUint32(evt[8:], wid)  // window
	// above-sibling = 0 (None)
	order.PutUint16(evt[16:], uint16(x))
	order.PutUint16(evt[18:], uint16(y))
	order.PutUint16(evt[20:], width)
	order.PutUint16(evt[22:], height)
	order.PutUint16(evt[24:], borderWidth)
	// override-redirect = 0
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

	// Reset pending-configure counts before synthesis so DrainPendingConfigures
	// later returns only ConfigureWindow requests from this reconnect window
	// (not counts accumulated during normal operation where they were balanced).
	for _, ac := range states {
		ac.ResetPendingConfigures()
	}

	for _, ac := range states {
		wins := ac.Windows()
		gcs := ac.GCs()
		pms := ac.Pixmaps()
		log.Printf("server: synthesising state for conn %d: %d windows %d GCs %d pixmaps %d cursors %d fonts",
			ac.ID, len(wins), len(gcs), len(pms), len(ac.Cursors()), len(ac.Fonts()))
		s.synthesiseAppConn(ac)
	}

	// Send SESSION_LIVE with the final seqNum for each connection. Between
	// synthesis start and now, the app may have sent requests that were
	// discarded (no client or synthActive=true). Those requests incremented
	// the server-side seqNum but never reached the real X server. The client
	// uses these final seqNums to compute the correct sequence-number offset so
	// forwarded replies/errors reach the app with the right sequence numbers.
	// Payload: [count:4 LE]([connID:4 LE][finalSeqNum:4 LE])...
	livePayload := make([]byte, 4+len(states)*8)
	binary.LittleEndian.PutUint32(livePayload[:4], uint32(len(states)))
	for i, ac := range states {
		binary.LittleEndian.PutUint32(livePayload[4+i*8:], ac.ID)
		binary.LittleEndian.PutUint32(livePayload[8+i*8:], ac.SeqNum())
	}
	log.Printf("server: synthesis complete, sending SESSION_LIVE for %d conns", len(states))
	s.WriteToClient(wire.MsgSessionLive, livePayload)

	// Clear live-relay block BEFORE injecting events. Draw commands triggered by
	// the app's Expose handler must not be discarded by sendMsgsToClient.
	s.mu.Lock()
	s.synthActive = false
	s.mu.Unlock()

	// Inject fake ShmCompletion for every known SHM segment. If the previous
	// proxxxy-client was killed between Xvfb processing a ShmPutImage and
	// forwarding the resulting ShmCompletion event, the app's SWGL compositor
	// is stuck waiting. The fake event unblocks it so it can resume rendering.
	for _, ac := range states {
		s.synthesisInjectShmCompletions(ac)
	}

	for _, ac := range states {
		// Drain pending configure counts and inject exactly that many
		// ConfigureNotify events. During the outage, the app may have sent N
		// ConfigureWindow requests; GTK3 increments configure_request_count by N
		// and only decrements on ConfigureNotify. Injecting fewer than N leaves
		// the count > 0 and permanently blocks drawing.
		pendingCfgs := ac.DrainPendingConfigures()
		s.synthesisConfigureNotify(ac, pendingCfgs)
		s.synthesisExpose(ac)
	}
}

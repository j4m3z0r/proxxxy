package client

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"

	"james.id.au/proxxxy/internal/compress"
	"james.id.au/proxxxy/internal/wire"
	"james.id.au/proxxxy/internal/x11"
)

// idRemap holds the mapping needed to translate resource IDs from an old X
// connection (from which synthesised state was captured) to a new one.
type idRemap struct {
	oldBase uint32
	oldMask uint32
	newBase uint32
	order   binary.ByteOrder
}

// remapGCValueList remaps resource-holding fields in a GC value-list.
// maskOff is the byte offset of the 4-byte value-mask; listOff is the byte
// offset of the first value. Remaps GCTile (bit 10), GCStipple (bit 11),
// GCFont (bit 14), and GCClipMask (bit 19) — all hold app-space resource IDs.
func remapGCValueList(out []byte, maskOff, listOff int, order binary.ByteOrder, remap func(int)) {
	if len(out) < maskOff+4 {
		return
	}
	valueMask := order.Uint32(out[maskOff:])
	for _, targetBit := range [4]uint{10, 11, 14, 19} {
		if valueMask&(1<<targetBit) == 0 {
			continue
		}
		off := listOff
		for b := uint(0); b < targetBit; b++ {
			if valueMask&(1<<b) != 0 {
				off += 4
			}
		}
		remap(off)
	}
}

// applyIDRemap rewrites resource IDs in cmd that belong to the old connection's
// range into the corresponding IDs in the new connection's range. Returns a
// modified copy; the original is not modified.
func applyIDRemap(cmd []byte, r idRemap) []byte {
	if len(cmd) < 4 {
		return cmd
	}
	out := make([]byte, len(cmd))
	copy(out, cmd)

	// BigRequest extension: length==0 in standard 2-byte field means the real
	// length follows as a 4-byte field, shifting all body fields by 4 bytes.
	s := 0
	if r.order.Uint16(cmd[2:4]) == 0 && len(cmd) >= 8 {
		s = 4
	}

	remap := func(off int) {
		if len(out) < off+4 {
			return
		}
		id := r.order.Uint32(out[off:])
		if id != 0 && id&^r.oldMask == r.oldBase {
			r.order.PutUint32(out[off:], r.newBase|(id&r.oldMask))
		}
	}

	opcode := cmd[0]
	switch opcode {
	case x11.OpcodeCreateWindow:
		remap(4 + s) // window ID
		remap(8 + s) // parent window ID
		// Remap CWCursor (bit 14) in value-list. Cursors are in app ridBase space.
		if len(out) >= 32+s {
			valueMask := r.order.Uint32(out[28+s : 32+s])
			if valueMask&(1<<14) != 0 {
				cursorOff := 32 + s
				for bit := uint(0); bit < 14; bit++ {
					if valueMask&(1<<bit) != 0 {
						cursorOff += 4
					}
				}
				remap(cursorOff)
			}
		}
	case x11.OpcodeChangeWindowAttributes:
		remap(4 + s) // window
		// Remap CWCursor (bit 14) in value-list.
		if len(out) >= 12+s {
			valueMask := r.order.Uint32(out[8+s : 12+s])
			if valueMask&(1<<14) != 0 {
				cursorOff := 12 + s
				for bit := uint(0); bit < 14; bit++ {
					if valueMask&(1<<bit) != 0 {
						cursorOff += 4
					}
				}
				remap(cursorOff)
			}
		}
	case x11.OpcodeGetWindowAttributes,
		x11.OpcodeDestroyWindow, x11.OpcodeMapWindow, x11.OpcodeUnmapWindow,
		x11.OpcodeMapSubwindows, x11.OpcodeUnmapSubwindows,
		x11.OpcodeConfigureWindow, x11.OpcodeGetGeometry, x11.OpcodeQueryTree,
		x11.OpcodeDeleteProperty, x11.OpcodeGetProperty,
		x11.OpcodeSetInputFocus, x11.OpcodeCirculateWindow,
		x11.OpcodeInstallColormap, x11.OpcodeUninstallColormap,
		x11.OpcodeQueryFont:
		remap(4 + s) // window / resource ID only
	case x11.OpcodeChangeProperty:
		remap(4 + s) // window (property atom at [8:12] is NOT a resource ID)
	case x11.OpcodeCreatePixmap:
		remap(4 + s) // pixmap ID
		remap(8 + s) // drawable
	case x11.OpcodeFreePixmap:
		remap(4 + s)
	case x11.OpcodeCreateGC:
		remap(4 + s) // GC ID
		remap(8 + s) // drawable
		// Remap GC value-list fields that hold resource IDs:
		//   bit 10: GCTile (pixmap), bit 11: GCStipple (pixmap),
		//   bit 14: GCFont (font),   bit 19: GCClipMask (pixmap, None=0 safe).
		if len(out) >= 16+s {
			remapGCValueList(out, 12+s, 16+s, r.order, remap)
		}
	case x11.OpcodeChangeGC:
		remap(4 + s) // GC ID
		// Same resource-bearing value-list fields as CreateGC.
		if len(out) >= 12+s {
			remapGCValueList(out, 8+s, 12+s, r.order, remap)
		}
	case x11.OpcodeFreeGC:
		remap(4 + s)
	case x11.OpcodeCopyGC:
		remap(4 + s) // src GC
		remap(8 + s) // dst GC
	case x11.OpcodeOpenFont, x11.OpcodeCloseFont:
		remap(4 + s) // font ID
	case x11.OpcodeCreateCursor:
		remap(4 + s)  // cursor ID
		remap(8 + s)  // source pixmap
		remap(12 + s) // mask pixmap (None=0 passes through safely)
	case x11.OpcodeCreateGlyphCursor:
		remap(4 + s)  // cursor ID
		remap(8 + s)  // source font
		remap(12 + s) // mask font (may be same as source or 0)
	case x11.OpcodeFreeCursor, x11.OpcodeRecolorCursor:
		remap(4 + s) // cursor ID
	case x11.OpcodeClearArea:
		remap(4 + s) // window
	case x11.OpcodeCopyArea, x11.OpcodeCopyPlane:
		remap(4 + s)  // src drawable
		remap(8 + s)  // dst drawable
		remap(12 + s) // GC
	case x11.OpcodeXInput:
		// XInput2 extension: minor opcode at byte[1] determines field layout.
		// XISelectEvents: window ID at [4]. Device IDs are not X resources.
		if len(cmd) >= 2 && cmd[1] == x11.XISelectEvents {
			remap(4 + s) // window
		}
	case x11.OpcodeMITSHM:
		// MIT-SHM extension: minor opcode at byte[1] determines field layout.
		// ShmAttach/ShmDetach: shmseg at [4]; default remap covers [4] and [8]
		// but shmid at [8] in ShmAttach is a Unix kernel ID, not an X resource
		// (its value is outside the app's ridBase range so the check is a no-op).
		// ShmPutImage (3): drawable[4], gc[8], shmseg[32].
		// ShmCreatePixmap (5): pixmap[4], drawable[8], shmseg[20].
		if len(cmd) < 2 {
			return out
		}
		switch cmd[1] {
		case x11.SHMAttach, x11.SHMDetach:
			remap(4 + s) // shmseg (ShmAttach: shmid at [8] is NOT a resource ID)
		default: // ShmPutImage (3), ShmGetImage (4), any future minor
			remap(4 + s) // drawable
			remap(8 + s) // gc / drawable
			remap(32 + s) // shmseg (ShmPutImage layout; ShmGetImage shmseg is at [28])
		case x11.SHMCreatePixmap: // minor 5
			remap(4 + s)  // pixmap ID
			remap(8 + s)  // drawable
			remap(20 + s) // shmseg
		}
	case x11.OpcodeRender:
		// RENDER extension: minor opcode at byte[1] determines field layout.
		// We remap Picture IDs and drawable IDs; PictFormat IDs are global
		// server constants (small values outside the app's resource range) and
		// pass through the range check unchanged.
		if len(cmd) < 2 {
			return out
		}
		switch cmd[1] {
		case x11.RenderCreatePicture:
			remap(4 + s) // pid
			remap(8 + s) // drawable
		case x11.RenderChangePicture:
			remap(4 + s) // pid
		case x11.RenderFreePicture:
			remap(4 + s) // pid
		case x11.RenderComposite:
			remap(8 + s)  // src picture
			remap(12 + s) // mask picture (None=0 passes through safely)
			remap(16 + s) // dst picture
		case x11.RenderTrapezoids, x11.RenderTriangles, x11.RenderTriStrip, x11.RenderTriFan:
			remap(8 + s)  // src picture
			remap(12 + s) // dst picture
		case x11.RenderSetPictureClipRectangles:
			remap(4 + s) // pic
		case x11.RenderSetPictureTransform:
			remap(4 + s) // pic
		case x11.RenderSetPictureFilter:
			remap(4 + s) // pic
		case x11.RenderCreateSolidFill, x11.RenderCreateLinearGradient,
			x11.RenderCreateRadialGradient, x11.RenderCreateConicalGradient:
			remap(4 + s) // pid only; no drawable
		// GlyphSet IDs are allocated from the app's ridBase/ridMask space and
		// need remapping when oldBase != newBase, just like other resource IDs.
		case x11.RenderCreateGlyphSet:
			remap(4 + s) // gsid
		case x11.RenderReferenceGlyphSet:
			remap(4 + s) // new gsid
			remap(8 + s) // existing gsid
		case x11.RenderFreeGlyphSet:
			remap(4 + s) // gsid
		case x11.RenderAddGlyphs:
			remap(4 + s) // gsid (glyph IDs within the set are CARD32 indices, not X resource IDs)
		// CompositeGlyphs wire layout (RENDER protocol):
		// [major:1][minor:1][len:2][op:1][pad:3][src:4][dst:4][mask-format:4][glyphset:4]...
		// src, dst, and glyphset are all in the app's resource-id space.
		case x11.RenderCompositeGlyphs8, x11.RenderCompositeGlyphs16, x11.RenderCompositeGlyphs32:
			remap(8 + s)  // src picture
			remap(12 + s) // dst picture
			remap(20 + s) // glyphset
		// FillRectangles: [op:1][pad:3][dst:4][color:8][rects…]
		case x11.RenderFillRectangles:
			remap(8 + s) // dst picture
		// CreateCursor: [major:1][27:1][len:2][cid:4][source_picture:4][x:2][y:2]
		case x11.RenderCreateCursor:
			remap(4 + s) // cursor ID (cid)
			remap(8 + s) // source picture
		}
	default:
		// Draw commands and anything else: drawable at [4], GC at [8].
		remap(4 + s)
		remap(8 + s)
	}
	return out
}

// applyEventReverseRemap translates resource IDs in an X11 event from the
// synthesis xconn's ID space (newBase) back to the app's original ID space
// (oldBase). This is the inverse of applyIDRemap: outgoing synthesis requests
// are forward-remapped (oldBase→newBase); incoming events must be
// reverse-remapped (newBase→oldBase) so the app recognises its own resources.
func applyEventReverseRemap(event []byte, r idRemap) []byte {
	if len(event) < 32 || r.oldBase == r.newBase {
		return event
	}
	out := make([]byte, len(event))
	copy(out, event)

	rev := func(off int) {
		if len(out) < off+4 {
			return
		}
		id := r.order.Uint32(out[off:])
		if id != 0 && id&^r.oldMask == r.newBase {
			r.order.PutUint32(out[off:], r.oldBase|(id&r.oldMask))
		}
	}

	evType := event[0] & 0x7f // strip SendEvent bit
	switch evType {
	case 2, 3: // KeyPress, KeyRelease: event=+4, root=+8 (server-owned, skip), child=+12
		rev(4)
		rev(12)
	case 4, 5, 6: // ButtonPress, ButtonRelease, MotionNotify
		rev(4)
		rev(12)
	case 7, 8: // EnterNotify, LeaveNotify
		rev(4)
		rev(12)
	case 9, 10: // FocusIn, FocusOut
		rev(4)
	case 12: // Expose
		rev(4)
	case 13: // GraphicsExposure: drawable=+4
		rev(4)
	case 15: // VisibilityNotify
		rev(4)
	case 16: // CreateNotify: parent=+4, window=+8
		rev(4)
		rev(8)
	case 17, 18, 19, 24, 26: // DestroyNotify, UnmapNotify, MapNotify, GravityNotify, CirculateNotify
		rev(4)
		rev(8)
	case 20: // MapRequest
		rev(4)
		rev(8)
	case 21: // ReparentNotify: event=+4, window=+8, parent=+12
		rev(4)
		rev(8)
		rev(12)
	case 22: // ConfigureNotify: event=+4, window=+8, above_sibling=+12
		rev(4)
		rev(8)
		rev(12)
	case 23: // ConfigureRequest: parent=+4, window=+8, sibling=+12
		rev(4)
		rev(8)
		rev(12)
	case 25: // ResizeRequest
		rev(4)
	case 27: // CirculateRequest: parent=+4, window=+8
		rev(4)
		rev(8)
	case 28: // PropertyNotify: window=+4 (atom at +8 is NOT a resource ID)
		rev(4)
	case 29: // SelectionClear: window=+4
		rev(4)
	case 30: // SelectionRequest: owner=+4, requestor=+8
		rev(4)
		rev(8)
	case 31: // SelectionNotify: requestor=+4
		rev(4)
	case 32: // ColormapNotify: window=+4, colormap=+8
		rev(4)
		rev(8)
	case 33: // ClientMessage: window=+4
		rev(4)
	case 35: // GenericEvent (XI2 etc.)
		// XIDeviceEvent layout (evtype 2-10): root=+20 (skip), event=+24, child=+28
		if len(event) >= 32 {
			evtype := r.order.Uint16(event[8:10])
			if evtype >= 2 && evtype <= 10 {
				rev(24)
				rev(28)
			}
		}
	}
	return out
}

// synthXconnState holds per-connID state needed during the synthesis phase.
type synthXconnState struct {
	ridBase     uint32
	ridMask     uint32
	rootWin     uint32
	order       binary.ByteOrder
	nextScratch uint32            // counts scratch IDs allocated, starting from ridMask downward
	colormaps   map[uint32]uint32 // visual → scratch colormap ID
}

// Client connects to a proxxxy-server and forwards X11 traffic to the local display.
type Client struct {
	serverAddr string

	server net.Conn
	srvW   sync.Mutex // serialises writes to server

	mu              sync.Mutex
	xConns          map[uint32]net.Conn
	decoders        map[uint32]*compress.Decoder
	idRemaps        map[uint32]idRemap          // resource ID remapping for synthesised connections
	synthDone       map[uint32]chan struct{}     // closed at SESSION_LIVE to switch synth relays to forward mode
	synthOrders     map[uint32]binary.ByteOrder // byte order for each synthesis xconn (needed to format GetInputFocus)
	synthFinalSeqNums map[uint32]uint32          // final app seqNum per conn, updated just before SESSION_LIVE

	// GPU extension suppression: track QueryExtension seqnums so DRI2/DRI3/Present
	// replies can be rewritten to "not present" before reaching the app.
	connSeqNums    map[uint32]uint32              // connID → seqnum of last request sent to real X server
	connSuppressed map[uint32]map[uint16]struct{} // connID → seqnums of suppressed QueryExtension requests
	connOrders     map[uint32]binary.ByteOrder    // connID → byte order (for reply stream parsing)

	// synthState and inSynthesis are accessed only from the main Run() goroutine.
	inSynthesis bool
	synthState  map[uint32]*synthXconnState // connID → synthesis xconn state
}

func New(serverAddr string) *Client {
	return &Client{
		serverAddr:        serverAddr,
		xConns:            make(map[uint32]net.Conn),
		decoders:          make(map[uint32]*compress.Decoder),
		idRemaps:          make(map[uint32]idRemap),
		synthDone:         make(map[uint32]chan struct{}),
		synthOrders:       make(map[uint32]binary.ByteOrder),
		synthFinalSeqNums: make(map[uint32]uint32),
		connSeqNums:       make(map[uint32]uint32),
		connSuppressed:    make(map[uint32]map[uint16]struct{}),
		connOrders:        make(map[uint32]binary.ByteOrder),
		synthState:        make(map[uint32]*synthXconnState),
	}
}

func (c *Client) Run() error {
	conn, err := net.Dial("tcp", c.serverAddr)
	if err != nil {
		return fmt.Errorf("connect to server: %w", err)
	}
	c.server = conn
	defer conn.Close()

	// Server sends HELLO first.
	msg, err := wire.Read(conn)
	if err != nil {
		return fmt.Errorf("read server HELLO: %w", err)
	}
	if msg.Type != wire.MsgHello {
		return fmt.Errorf("read server HELLO: unexpected message type 0x%02x", msg.Type)
	}
	version, err := wire.ReadHello(msg.Payload)
	if err != nil {
		return err
	}
	if version != wire.ProtocolVersion {
		return fmt.Errorf("version mismatch: server=%d client=%d", version, wire.ProtocolVersion)
	}
	if err := wire.WriteHello(conn); err != nil {
		return fmt.Errorf("write HELLO: %w", err)
	}

	// SESSION_RESUME phase: consume synthesised state.
	// synthRelay goroutines run immediately but stay in drain mode until SESSION_LIVE.
	c.inSynthesis = true
	for {
		msg, err = wire.Read(conn)
		if err != nil {
			return fmt.Errorf("read session init: %w", err)
		}
		if msg.Type == wire.MsgSessionLive {
			break
		}
		switch msg.Type {
		case wire.MsgX11Setup:
			c.handleX11Setup(msg.Payload)
		case wire.MsgX11Data:
			c.handleX11Data(msg.Payload)
		case wire.MsgDictDefine, wire.MsgDictRef, wire.MsgDictExpire, wire.MsgTemplateDefine, wire.MsgTemplateApply:
			c.handleCompressed(msg)
		}
	}
	c.inSynthesis = false

	// SESSION_LIVE: switch synthesis relays to live-forward mode.
	// The payload carries the final seqNum per connID so synthRelay can
	// compute the correct seqOffset, accounting for requests the app sent
	// during synthesis that were discarded (never reached the real X server).
	// Payload format: [count:4 LE]([connID:4 LE][finalSeqNum:4 LE])...
	//
	// Send GetInputFocus to each synthesis xconn HERE, before entering the live
	// message loop. This guarantees GetInputFocus is the first request the real X
	// server sees after synthesis commands — preventing a race where a live query
	// from the app (triggered by the Expose we inject) could arrive at the
	// synthesis xconn first and cause synthRelay Phase 2 to capture the wrong
	// nSynth, making seqOffset wrong and breaking all subsequent reply routing.
	livePayload := msg.Payload
	c.mu.Lock()
	if len(livePayload) >= 4 {
		count := int(binary.LittleEndian.Uint32(livePayload[:4]))
		for i := 0; i < count && 4+i*8+8 <= len(livePayload); i++ {
			cid := binary.LittleEndian.Uint32(livePayload[4+i*8:])
			seq := binary.LittleEndian.Uint32(livePayload[8+i*8:])
			c.synthFinalSeqNums[cid] = seq
		}
	}
	for connID, done := range c.synthDone {
		if xconn, ok := c.xConns[connID]; ok {
			ord := c.synthOrders[connID]
			barrier := [4]byte{x11.OpcodeGetInputFocus, 0}
			ord.PutUint16(barrier[2:], 1)
			xconn.Write(barrier[:]) //nolint:errcheck
		}
		close(done)
	}
	c.synthDone = make(map[uint32]chan struct{})
	c.synthOrders = make(map[uint32]binary.ByteOrder)
	c.mu.Unlock()
	c.synthState = make(map[uint32]*synthXconnState)

	log.Println("client: live")

	for {
		msg, err := wire.Read(conn)
		if err != nil {
			return fmt.Errorf("server gone: %w", err)
		}
		switch msg.Type {
		case wire.MsgX11Data:
			c.handleX11Data(msg.Payload)
		case wire.MsgDictDefine, wire.MsgDictRef, wire.MsgDictExpire, wire.MsgTemplateDefine, wire.MsgTemplateApply:
			c.handleCompressed(msg)
		}
	}
}

// handleX11Setup handles a MsgX11Setup message received during SESSION_RESUME.
// It establishes a new X connection, performs the setup handshake (consuming
// the server's reply without forwarding it), extracts the new resource-id-base
// for ID remapping, and defers the relay goroutine start until SESSION_LIVE.
func (c *Client) handleX11Setup(payload []byte) {
	if len(payload) < 16 {
		log.Println("client: X11Setup payload too short")
		return
	}
	connID := binary.LittleEndian.Uint32(payload[0:4])
	oldBase := binary.LittleEndian.Uint32(payload[4:8])
	oldMask := binary.LittleEndian.Uint32(payload[8:12])
	appSeqNum := binary.LittleEndian.Uint32(payload[12:16])
	setupBytes := payload[16:]
	if len(setupBytes) == 0 {
		return
	}

	xconn, err := dialX11(localDisplay())
	if err != nil {
		log.Println("client: dial X for synthesis:", err)
		return
	}

	// Forward the setup request to the real X server.
	if _, err := xconn.Write(setupBytes); err != nil {
		log.Println("client: write setup to X:", err)
		xconn.Close()
		return
	}

	// Read and consume the setup reply (to complete the handshake).
	// We do NOT forward it to the server/app — the app still has its original
	// setup state from the previous session.
	newBase, newMask, rootWin, err := readAndConsumeSetupReply(xconn, setupBytes[0])
	if err != nil {
		log.Println("client: read X setup reply:", err)
		xconn.Close()
		return
	}

	var order binary.ByteOrder = binary.LittleEndian
	if setupBytes[0] == 0x42 {
		order = binary.BigEndian
	}

	// Enable BigRequests on the synthesis xconn so that GIMP's large requests
	// (and their DrawCmd replays) are accepted by the real X server.
	if err := enableBigRequests(xconn, order); err != nil {
		log.Println("client: enableBigRequests:", err)
		xconn.Close()
		return
	}

	c.mu.Lock()
	// Close any existing xconn for this connID so its synthRelay goroutine
	// exits promptly. Without this, the old goroutine keeps forwarding events
	// with stale sequence offsets onto the new connection, corrupting Xlib's
	// sequence tracking and causing "Unknown sequence number" assertion failures.
	if old, ok := c.xConns[connID]; ok {
		old.Close()
	}
	c.xConns[connID] = xconn
	// enableBigRequests sent 2 requests (QueryExtension + BigReqEnable) before
	// handleX11Data can be called for this connID. Pre-seed the seqnum counter
	// so subsequent handleX11Data calls assign the correct seqnums.
	c.connSeqNums[connID] = 2
	c.connOrders[connID] = order
	if oldBase != 0 && newBase != 0 && oldBase != newBase {
		log.Printf("client: synthesis conn %d: oldBase=0x%08x mask=0x%08x newBase=0x%08x",
			connID, oldBase, oldMask, newBase)
		c.idRemaps[connID] = idRemap{oldBase, oldMask, newBase, order}
	} else {
		log.Printf("client: synthesis conn %d: oldBase=0x%08x newBase=0x%08x (no remap needed)",
			connID, oldBase, newBase)
		delete(c.idRemaps, connID)
	}
	// synthRelay runs immediately in drain mode, discarding synthesis-phase
	// events/errors whose sequence numbers would confuse the app's Xlib. Once
	// SESSION_LIVE is signalled via done, it switches to forwarding mode:
	// events (type ≥ 2) reach the app; errors (type 0) and replies (type 1)
	// are still discarded because their sequence numbers are synthesis-internal.
	done := make(chan struct{})
	c.synthDone[connID] = done
	c.synthOrders[connID] = order
	c.synthFinalSeqNums[connID] = appSeqNum // initial; updated by MsgSessionLive
	c.mu.Unlock()
	// synthState is main-goroutine-only; no mutex needed.
	c.synthState[connID] = &synthXconnState{
		ridBase:   newBase,
		ridMask:   newMask,
		rootWin:   rootWin,
		order:     order,
		colormaps: make(map[uint32]uint32),
	}
	go c.synthRelay(connID, xconn, done, order, appSeqNum)
}

// enableBigRequests sends QueryExtension("BIG-REQUESTS") + BigReqEnable on
// xconn so that the connection accepts requests larger than 65535×4 bytes.
// Silently succeeds if the extension is absent.
func enableBigRequests(xconn net.Conn, order binary.ByteOrder) error {
	const name = "BIG-REQUESTS"
	nameLen := len(name) // 12 — already 4-byte aligned, no padding needed
	reqWords := uint16((4 + 2 + 2 + nameLen) / 4)
	buf := make([]byte, 4+2+2+nameLen)
	buf[0] = x11.OpcodeQueryExtension
	buf[1] = 0
	order.PutUint16(buf[2:4], reqWords)
	order.PutUint16(buf[4:6], uint16(nameLen))
	// buf[6:8] already zero (pad)
	copy(buf[8:], name)
	if _, err := xconn.Write(buf); err != nil {
		return err
	}
	var reply [32]byte
	if _, err := io.ReadFull(xconn, reply[:]); err != nil {
		return err
	}
	if reply[0] != 1 || reply[8] == 0 {
		return nil // extension absent
	}
	majorOpcode := reply[9]
	enable := [4]byte{majorOpcode, 0}
	order.PutUint16(enable[2:], 1)
	if _, err := xconn.Write(enable[:]); err != nil {
		return err
	}
	if _, err := io.ReadFull(xconn, reply[:]); err != nil {
		return err
	}
	return nil
}

// readAndConsumeSetupReply reads and discards one X11 server setup reply from
// conn, returning the new resource-id-base, resource-id-mask, and root window.
// readSetupReply reads a complete X11 connection setup reply from conn and
// returns the raw bytes. Used for pending connections (ridBase==0) where the
// reply must be forwarded to the waiting app, not consumed.
func readSetupReply(conn net.Conn, byteOrderByte byte) ([]byte, error) {
	var order binary.ByteOrder = binary.LittleEndian
	if byteOrderByte == 0x42 {
		order = binary.BigEndian
	}
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return nil, fmt.Errorf("read setup reply header: %w", err)
	}
	var bodyLen int
	if hdr[0] == 1 { // success: length at bytes[6:8] in 4-byte words
		bodyLen = int(order.Uint16(hdr[6:8])) * 4
	} else { // failed (0) or authenticate (2): reason string length in hdr[1]
		bodyLen = int(hdr[1])
	}
	body := make([]byte, bodyLen)
	if bodyLen > 0 {
		if _, err := io.ReadFull(conn, body); err != nil {
			return nil, fmt.Errorf("read setup reply body: %w", err)
		}
	}
	result := make([]byte, 8+bodyLen)
	copy(result, hdr)
	copy(result[8:], body)
	return result, nil
}

func readAndConsumeSetupReply(conn net.Conn, byteOrderByte byte) (ridBase, ridMask, rootWin uint32, err error) {
	hdr := make([]byte, 8)
	if _, err = io.ReadFull(conn, hdr); err != nil {
		return 0, 0, 0, fmt.Errorf("read setup reply header: %w", err)
	}
	var order binary.ByteOrder = binary.LittleEndian
	if byteOrderByte == 0x42 {
		order = binary.BigEndian
	}
	if hdr[0] != 1 {
		// Failed or authenticate — read reason and discard.
		reasonLen := int(hdr[1])
		io.ReadFull(conn, make([]byte, reasonLen)) //nolint:errcheck
		return 0, 0, 0, fmt.Errorf("X11 setup rejected (code %d)", hdr[0])
	}
	dataLen := int(order.Uint16(hdr[6:8])) * 4
	data := make([]byte, dataLen)
	if _, err = io.ReadFull(conn, data); err != nil {
		return 0, 0, 0, fmt.Errorf("read setup reply data: %w", err)
	}
	if dataLen < 12 {
		return 0, 0, 0, fmt.Errorf("setup reply data too short")
	}
	// Additional data layout:
	//   [0:4]   release-number
	//   [4:8]   resource-id-base
	//   [8:12]  resource-id-mask
	//   [12:16] motion-buffer-size
	//   [16:18] vendor-length
	//   [18:20] max-request-length
	//   [20]    number-of-screens
	//   [21]    number-of-formats
	//   [22:32] image/bitmap format bytes + padding
	//   [32:]   vendor string (vendor-length bytes, padded to 4)
	//   [32+pad(vendorLen)+numFormats*8:] first screen (first 4 bytes = root window)
	ridBase = order.Uint32(data[4:8])
	ridMask = order.Uint32(data[8:12])
	if dataLen >= 22 {
		vendorLen := int(order.Uint16(data[16:18]))
		numFormats := int(data[21])
		vendorPadded := (vendorLen + 3) &^ 3
		screenOff := 32 + vendorPadded + numFormats*8
		if dataLen >= screenOff+4 {
			rootWin = order.Uint32(data[screenOff:])
		}
	}
	return ridBase, ridMask, rootWin, nil
}

func (c *Client) handleX11Data(payload []byte) {
	connID, data, err := wire.ParseX11Data(payload)
	if err != nil {
		log.Println("client: parse X11Data:", err)
		return
	}

	c.mu.Lock()
	xconn, ok := c.xConns[connID]
	remap, hasRemap := c.idRemaps[connID]
	c.mu.Unlock()

	isNewConn := false
	if !ok {
		xconn, err = dialX11(localDisplay())
		if err != nil {
			log.Println("client: dial local display:", err)
			return
		}
		c.mu.Lock()
		c.xConns[connID] = xconn
		// Record byte order from the setup request byte-order indicator.
		if len(data) > 0 {
			if data[0] == 0x42 {
				c.connOrders[connID] = binary.BigEndian
			} else {
				c.connOrders[connID] = binary.LittleEndian
			}
		}
		c.mu.Unlock()

		// The first data for a new connection is always the X11 connection setup
		// request (byte-order indicator 0x6C or 0x42). We must write it, read the
		// setup reply, and forward the reply to the server (which delivers it to the
		// waiting app) before starting relayXToServer. Without this, relayXToServer
		// would read the setup reply and misinterpret its length field (a CARD16 at
		// bytes[6:8], not the CARD32 at bytes[4:8] that regular replies use), trying
		// to read hundreds of megabytes and hanging forever.
		if len(data) > 0 && (data[0] == 0x6C || data[0] == 0x42) {
			if _, writeErr := xconn.Write(data); writeErr != nil {
				log.Println("client: write setup to display:", writeErr)
				c.mu.Lock()
				delete(c.xConns, connID)
				c.mu.Unlock()
				xconn.Close()
				return
			}
			reply, replyErr := readSetupReply(xconn, data[0])
			if replyErr != nil {
				log.Println("client: read setup reply:", replyErr)
				c.mu.Lock()
				delete(c.xConns, connID)
				c.mu.Unlock()
				xconn.Close()
				return
			}
			c.srvW.Lock()
			fwdErr := wire.WriteX11Data(c.server, connID, reply)
			c.srvW.Unlock()
			if fwdErr != nil {
				log.Println("client: forward setup reply to server:", fwdErr)
				c.mu.Lock()
				delete(c.xConns, connID)
				c.mu.Unlock()
				xconn.Close()
				return
			}
			go c.relayXToServer(connID, xconn)
			return
		}

		go c.relayXToServer(connID, xconn)
		hasRemap = false
		isNewConn = true
	}

	if hasRemap {
		data = applyIDRemap(data, remap)
	}
	// During synthesis, inject a scratch colormap into CreateWindow requests that
	// have a non-default depth but no CWColormap. The server strips the original
	// colormap (it belongs to a dead connection), but the X server requires an
	// explicit colormap for depth != root depth. We create one on the fly.
	if c.inSynthesis && len(data) >= 32 && data[0] == x11.OpcodeCreateWindow {
		if state, ok := c.synthState[connID]; ok {
			data = c.injectColormap(data, state, xconn)
		}
	}
	if _, err := xconn.Write(data); err != nil {
		log.Println("client: write to display:", err)
		return
	}

	// Track seqnum and detect QueryExtension requests for GPU extensions.
	// Setup requests (byte-order indicator 0x6C or 0x42) are not numbered —
	// skip them. isNewConn implies the first write was the setup request.
	isSetup := isNewConn || (len(data) > 0 && (data[0] == 0x6C || data[0] == 0x42))
	if !isSetup {
		c.mu.Lock()
		c.connSeqNums[connID]++
		seqnum := uint16(c.connSeqNums[connID])
		if len(data) >= 8 && data[0] == x11.OpcodeQueryExtension {
			order := c.connOrders[connID]
			if order == nil {
				order = binary.LittleEndian
			}
			nameLen := int(order.Uint16(data[4:6]))
			if len(data) >= 8+nameLen {
				name := string(data[8 : 8+nameLen])
				if isGPUExtension(name) {
					if c.connSuppressed[connID] == nil {
						c.connSuppressed[connID] = make(map[uint16]struct{})
					}
					c.connSuppressed[connID][seqnum] = struct{}{}
				}
			}
		}
		c.mu.Unlock()
	}
}

// isGPUExtension reports whether the named X11 extension cannot be forwarded over
// proxxxy's TCP wire protocol and must be suppressed (QueryExtension rewritten to
// "not present") so apps fall back to wire-friendly paths.
//
//   - DRI2/DRI3/Present pass GPU dma-buf file descriptors via SCM_RIGHTS ancillary
//     data, which cannot cross a TCP relay → apps fall back to software GL.
//
// NOTE: MIT-SHM is intentionally NOT suppressed. Suppressing it globally
// regresses the local case (app, server and X server on one host, e.g. the Xvfb
// E2E tests), where GIMP relies on shared-memory rendering and does not fall back
// to XPutImage cleanly. On a remote link ShmAttach simply fails (the segment is
// on another host) and apps fall back to RENDER/XPutImage on their own, so no
// suppression is needed — what mattered was tracking SYNC/MIT-SHM/XInput by their
// real (server-assigned) opcodes; see internal/x11 extByOpcode.
func isGPUExtension(name string) bool {
	switch name {
	case "DRI2", "DRI3", "Present":
		return true
	}
	return false
}

func (c *Client) handleCompressed(msg wire.Msg) {
	if len(msg.Payload) < 4 {
		log.Println("client: compressed msg payload too short")
		return
	}
	connID := binary.LittleEndian.Uint32(msg.Payload[:4])

	c.mu.Lock()
	dec, ok := c.decoders[connID]
	if !ok {
		dec = compress.NewDecoder()
		c.decoders[connID] = dec
	}
	c.mu.Unlock()

	_, data, err := dec.Decode(msg)
	if err != nil {
		log.Println("client: decode:", err)
		return
	}
	if data == nil {
		return // define/expire — nothing to forward
	}

	c.mu.Lock()
	xconn, ok := c.xConns[connID]
	remap, hasRemap := c.idRemaps[connID]
	c.mu.Unlock()

	if !ok {
		var dialErr error
		xconn, dialErr = dialX11(localDisplay())
		if dialErr != nil {
			log.Println("client: dial local display:", dialErr)
			return
		}
		c.mu.Lock()
		c.xConns[connID] = xconn
		c.mu.Unlock()
		go c.relayXToServer(connID, xconn)
		hasRemap = false
	}

	if hasRemap {
		data = applyIDRemap(data, remap)
	}
	if _, err := xconn.Write(data); err != nil {
		log.Println("client: write to display:", err)
	}
}

// synthRelay manages the X connection created for a synthesised app connection.
// It runs in three phases:
//
//  1. Drain (SESSION_RESUME): discard all messages from the synthesis conn.
//     Synthesis requests (CreateWindow, MapWindow, etc.) generate events with
//     synthesis-internal sequence numbers that would confuse the app's Xlib.
//
//  2. Barrier (after SESSION_LIVE): send GetInputFocus and drain until its
//     reply arrives, flushing any remaining synthesis-phase events.
//     Capture the reply's sequence number (N_synth) so we can compute the
//     offset needed to map synthesis xconn seq numbers to app seq numbers.
//
//  3. Forward: relay all subsequent messages back to the server (which routes
//     them to the app), rewriting sequence numbers: new_seq = old_seq + offset,
//     where offset = uint16(appSeqNum) - N_synth. This keeps the app's Xlib
//     seq counter consistent as live requests flow through the synthesis conn.
// injectColormap adds a scratch colormap to a CreateWindow request if it
// specifies a non-default depth and visual but has no CWColormap in its
// value-mask. This prevents BadMatch errors when the X server requires an
// explicit colormap for windows with a non-root depth (e.g., 32-bit ARGB).
// The scratch colormap is created on xconn and cached by visual in state.
func (c *Client) injectColormap(data []byte, state *synthXconnState, xconn net.Conn) []byte {
	if len(data) < 32 || data[0] != x11.OpcodeCreateWindow {
		return data
	}
	depth := data[1]
	if depth == 0 {
		return data // CopyFromParent — server picks appropriate colormap
	}
	valueMask := state.order.Uint32(data[28:32])
	if valueMask&(1<<13) != 0 {
		return data // CWColormap already present
	}
	visual := state.order.Uint32(data[24:28])
	if visual == 0 {
		return data // CopyFromParent visual
	}

	cmapID, ok := state.colormaps[visual]
	if !ok {
		// Allocate scratch ID from the top of the synthesis xconn's rid range,
		// counting downward to avoid collisions with synthesised resource IDs
		// (which start from the low bits of ridMask).
		cmapID = state.ridBase | (state.ridMask - state.nextScratch)
		state.nextScratch++

		// CreateColormap: [opcode:1][alloc:1][len:2][cmap:4][window:4][visual:4]
		buf := make([]byte, 16)
		buf[0] = x11.OpcodeCreateColormap
		buf[1] = 0 // alloc=None
		state.order.PutUint16(buf[2:4], 4)
		state.order.PutUint32(buf[4:8], cmapID)
		state.order.PutUint32(buf[8:12], state.rootWin)
		state.order.PutUint32(buf[12:16], visual)
		if _, err := xconn.Write(buf); err != nil {
			log.Printf("client: injectColormap: CreateColormap visual=0x%x: %v", visual, err)
			return data
		}
		state.colormaps[visual] = cmapID
	}
	return addColormapToCreateWindow(data, cmapID, state.order)
}

// addColormapToCreateWindow inserts CWColormap (bit 13) with the given
// colormap ID into the CreateWindow request's value-list at the correct
// position, and updates the value-mask and length fields.
func addColormapToCreateWindow(data []byte, cmapID uint32, order binary.ByteOrder) []byte {
	if len(data) < 32 {
		return data
	}
	valueMask := order.Uint32(data[28:32])

	// Count set bits below bit 13 to find the insertion index in the value-list.
	insertPos := 0
	for bit := uint(0); bit < 13; bit++ {
		if valueMask&(1<<bit) != 0 {
			insertPos++
		}
	}
	insertOffset := 32 + insertPos*4
	if insertOffset > len(data) {
		return data
	}

	newData := make([]byte, len(data)+4)
	copy(newData[:insertOffset], data[:insertOffset])
	order.PutUint32(newData[insertOffset:], cmapID)
	copy(newData[insertOffset+4:], data[insertOffset:])

	order.PutUint32(newData[28:32], valueMask|(1<<13))
	order.PutUint16(newData[2:4], order.Uint16(newData[2:4])+1)
	return newData
}

func (c *Client) synthRelay(connID uint32, xconn net.Conn, done <-chan struct{}, order binary.ByteOrder, appSeqNum uint32) {
	defer func() {
		xconn.Close()
		c.mu.Lock()
		// Only remove this connID if it still points to our xconn. On a second
		// reconnect, handleX11Setup may have already replaced xConns[connID] with
		// a new xconn; deleting it here would evict the new connection.
		if c.xConns[connID] == xconn {
			delete(c.xConns, connID)
			delete(c.idRemaps, connID)
		}
		c.mu.Unlock()
	}()

	type synthMsg struct {
		hdr  [32]byte
		tail []byte
		err  error
	}
	msgs := make(chan synthMsg, 16)
	go func() {
		var h [32]byte
		for {
			if _, err := io.ReadFull(xconn, h[:]); err != nil {
				msgs <- synthMsg{err: err}
				return
			}
			var tail []byte
			if h[0] == 1 || h[0] == 35 { // reply or GenericEvent has variable tail
				n := int(order.Uint32(h[4:8])) * 4
				if n > 0 {
					tail = make([]byte, n)
					if _, err := io.ReadFull(xconn, tail); err != nil {
						msgs <- synthMsg{err: err}
						return
					}
				}
			}
			cp := synthMsg{tail: tail}
			copy(cp.hdr[:], h[:])
			msgs <- cp
		}
	}()

	// Events that arrive during synthesis and barrier phases carry
	// synthesis-internal sequence numbers. Only FocusIn/FocusOut are worth
	// replaying: the WM sends FocusIn after MapWindow and losing it means the
	// app never receives keyboard focus. All other WM management events
	// (ConfigureNotify, MapNotify, ReparentNotify, Expose, PropertyNotify, …)
	// are filtered out because they carry display :1 geometry/state that does
	// not match display :2, and forwarding them corrupts GTK3/GDK internal
	// state (e.g. ConfigureNotify triggers thaw on a window that was never
	// frozen, breaking the update-freeze accounting and preventing redraws).
	var bufferedEvents []synthMsg

	// synthShouldBuffer returns true for events that must survive synthesis.
	// Only FocusIn (9) and FocusOut (10) qualify; everything else is dropped.
	synthShouldBuffer := func(evType byte) bool {
		return evType == 9 || evType == 10
	}

	// Phase 1: drain until SESSION_LIVE fires. Transition immediately on done
	// so we do not consume and discard the first live reply (e.g. from an
	// XSync the app issues in its Expose handler).
	for {
		select {
		case <-done:
			goto phase2
		case m := <-msgs:
			if m.err != nil {
				return
			}
			if m.hdr[0] == 0 {
				badID := order.Uint32(m.hdr[4:8])
				minor := order.Uint16(m.hdr[8:10])
				log.Printf("client: synthRelay conn %d: X error during synthesis: code=%d major=%d minor=%d badID=0x%08x",
					connID, m.hdr[1], m.hdr[10], minor, badID)
			} else if m.hdr[0] >= 2 && synthShouldBuffer(m.hdr[0]) {
				bufferedEvents = append(bufferedEvents, m)
			}
		}
	}
phase2:

	// Phase 2: drain until we receive the GetInputFocus barrier reply.
	// GetInputFocus was already sent by serve() when it processed SESSION_LIVE,
	// before entering the live message loop — guaranteeing it arrived at the real
	// X server before any live queries from the app. We just wait for the reply
	// here; its sequence number (N_synth) lets us compute seqOffset.
	var nSynth uint16
	for {
		m := <-msgs
		if m.err != nil {
			return
		}
		if m.hdr[0] == 0 {
			badID := order.Uint32(m.hdr[4:8])
			log.Printf("client: synthRelay conn %d: X error during barrier: code=%d major=%d badID=0x%08x",
				connID, m.hdr[1], m.hdr[10], badID)
		} else if m.hdr[0] == 1 {
			nSynth = order.Uint16(m.hdr[2:4])
			break
		} else if m.hdr[0] >= 2 && synthShouldBuffer(m.hdr[0]) {
			bufferedEvents = append(bufferedEvents, m)
		}
	}

	// Phase 3: forward all messages back to the server with rewritten sequence
	// numbers. seqOffset maps synthesis-xconn seq space to app seq space so
	// Xlib on the app side recognises replies to its own requests.
	// Use the FINAL seqNum (updated in MsgSessionLive) which accounts for
	// requests the app sent during synthesis that were discarded server-side
	// (never forwarded to the real X server, but still incremented the app's
	// sequence counter). Using the initial seqNum would leave seqOffset off by
	// the number of discarded requests, causing all forwarded replies to arrive
	// with wrong sequence numbers and Xlib matching them to wrong requests.
	c.mu.Lock()
	finalSeqNum := c.synthFinalSeqNums[connID]
	evRemap, hasEvRemap := c.idRemaps[connID]
	c.mu.Unlock()
	seqOffset := uint16(finalSeqNum) - nSynth

	// Helper to assemble a full message buffer from header + tail, applying
	// reverse event remapping when the ridBase changed between reconnects.
	assembleAndRemap := func(h *[32]byte, tail []byte) []byte {
		var full []byte
		if len(tail) > 0 {
			full = make([]byte, 32+len(tail))
			copy(full, h[:])
			copy(full[32:], tail)
		} else {
			full = h[:]
		}
		// Reverse-remap resource IDs in events (type ≥ 2): Xvfb delivers events
		// using the synthesis xconn's ridBase (newBase). Translate back to the
		// app's original ridBase (oldBase) so the app recognises its own windows.
		if hasEvRemap && full[0] >= 2 {
			full = applyEventReverseRemap(full, evRemap)
		}
		return full
	}

	// Helper to forward one message with seq rewriting applied.
	forward := func(m synthMsg) {
		if m.hdr[0] == 0 {
			// Discard errors whose seqNum predates or equals the barrier: these are
			// late-arriving synthesis errors that missed Phase 2 draining. Forwarding
			// them with the seqOffset applied would map them to a live seqNum, causing
			// the app to associate the error with one of its own (innocent) requests.
			seq := order.Uint16(m.hdr[2:4])
			if seq <= nSynth {
				return
			}
			badID := order.Uint32(m.hdr[4:8])
			minor := order.Uint16(m.hdr[8:10])
			log.Printf("client: synthRelay conn %d: X error during live: code=%d major=%d minor=%d badID=0x%08x",
				connID, m.hdr[1], m.hdr[10], minor, badID)
		}
		h := m.hdr
		order.PutUint16(h[2:4], order.Uint16(h[2:4])+seqOffset)
		full := assembleAndRemap(&h, m.tail)
		c.srvW.Lock()
		wire.WriteX11Data(c.server, connID, full) //nolint:errcheck
		c.srvW.Unlock()
	}

	// Replay buffered FocusIn/FocusOut events from synthesis/barrier phases.
	// We cannot use seqOffset here: synthesis seq numbers after offset map to
	// values LESS THAN the seq the app last saw before disconnect. XCB in
	// XInitThreads mode requires non-decreasing sequence numbers and crashes
	// (xcb_xlib_threads_sequence_lost) on a backwards jump. Stamp all buffered
	// events with uint16(finalSeqNum) — the watermark at SESSION_LIVE — so the
	// app sees them as arriving "now". Phase 3 events increment normally after.
	appSeq16 := uint16(finalSeqNum)
	for _, m := range bufferedEvents {
		h := m.hdr
		order.PutUint16(h[2:4], appSeq16)
		full := assembleAndRemap(&h, m.tail)
		c.srvW.Lock()
		wire.WriteX11Data(c.server, connID, full) //nolint:errcheck
		c.srvW.Unlock()
	}
	bufferedEvents = nil

	for {
		m := <-msgs
		if m.err != nil {
			return
		}
		forward(m)
	}
}

func (c *Client) relayXToServer(connID uint32, xconn net.Conn) {
	defer func() {
		xconn.Close()
		c.mu.Lock()
		delete(c.xConns, connID)
		delete(c.decoders, connID)
		delete(c.idRemaps, connID)
		delete(c.connSeqNums, connID)
		delete(c.connSuppressed, connID)
		delete(c.connOrders, connID)
		c.mu.Unlock()
	}()
	// Parse the X11 reply/event/error stream message-by-message so we can
	// intercept QueryExtension replies for GPU extensions (DRI2/DRI3/Present)
	// and rewrite them to "not present" before forwarding to the app.
	//
	// X11 message sizes from the server:
	//   byte[0] == 0: error          → always 32 bytes
	//   byte[0] == 1: reply          → 32 + uint32(bytes[4:8], LE) * 4 bytes
	//   byte[0] == 35: GenericEvent  → 32 + uint32(bytes[4:8], LE) * 4 bytes
	//   other:         event         → always 32 bytes
	r := bufio.NewReaderSize(xconn, 32*1024)
	hdr := make([]byte, 32)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			return
		}
		var tail []byte
		if hdr[0] == 1 || hdr[0] == 35 { // reply or GenericEvent: has variable-length tail
			extra := binary.LittleEndian.Uint32(hdr[4:8]) * 4
			if extra > 0 {
				tail = make([]byte, extra)
				if _, err := io.ReadFull(r, tail); err != nil {
					return
				}
			}
		}
		if hdr[0] == 1 { // reply: check for suppressed QueryExtension
			seqnum := binary.LittleEndian.Uint16(hdr[2:4])
			c.mu.Lock()
			_, suppress := c.connSuppressed[connID][seqnum]
			if suppress {
				delete(c.connSuppressed[connID], seqnum)
			}
			c.mu.Unlock()
			if suppress {
				// Rewrite QueryExtension reply: extension not present.
				hdr[8] = 0  // present = false
				hdr[9] = 0  // major-opcode = 0
				hdr[10] = 0 // first-event = 0
				hdr[11] = 0 // first-error = 0
			}
		}
		var full []byte
		if len(tail) > 0 {
			full = make([]byte, 32+len(tail))
			copy(full, hdr)
			copy(full[32:], tail)
		} else {
			full = append([]byte(nil), hdr...)
		}
		c.srvW.Lock()
		werr := wire.WriteX11Data(c.server, connID, full)
		c.srvW.Unlock()
		if werr != nil {
			return
		}
	}
}

func localDisplay() string {
	if d := os.Getenv("DISPLAY"); d != "" {
		return d
	}
	return ":0"
}

// dialX11 opens a connection to the X display at the given display string.
// Supports:
//   - :N and :N.S  — standard Unix socket at /tmp/.X11-unix/XN
//   - /path/to/socket:N — XQuartz/macOS format; the socket file is the path before the last colon
func dialX11(display string) (net.Conn, error) {
	if len(display) == 0 {
		return nil, fmt.Errorf("unsupported display format: %q", display)
	}
	if display[0] == '/' {
		// XQuartz on macOS sets DISPLAY to the Unix socket path directly,
		// with the display number as part of the filename (e.g. "...xquartz:0").
		return net.Dial("unix", display)
	}
	if display[0] != ':' {
		return nil, fmt.Errorf("unsupported display format: %q", display)
	}
	num := display[1:]
	for i, ch := range num {
		if ch == '.' {
			num = num[:i]
			break
		}
	}
	return net.Dial("unix", fmt.Sprintf("/tmp/.X11-unix/X%s", num))
}

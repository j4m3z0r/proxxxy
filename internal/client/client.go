package client

import (
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

// applyIDRemap rewrites resource IDs in cmd that belong to the old connection's
// range into the corresponding IDs in the new connection's range. Returns a
// modified copy; the original is not modified.
func applyIDRemap(cmd []byte, r idRemap) []byte {
	if len(cmd) < 4 {
		return cmd
	}
	out := make([]byte, len(cmd))
	copy(out, cmd)

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
		remap(4)  // window ID
		remap(8)  // parent window ID
	case x11.OpcodeChangeWindowAttributes, x11.OpcodeGetWindowAttributes,
		x11.OpcodeDestroyWindow, x11.OpcodeMapWindow, x11.OpcodeUnmapWindow,
		x11.OpcodeMapSubwindows, x11.OpcodeUnmapSubwindows,
		x11.OpcodeConfigureWindow, x11.OpcodeGetGeometry, x11.OpcodeQueryTree,
		x11.OpcodeDeleteProperty, x11.OpcodeGetProperty,
		x11.OpcodeSetInputFocus, x11.OpcodeCirculateWindow,
		x11.OpcodeInstallColormap, x11.OpcodeUninstallColormap,
		x11.OpcodeQueryFont:
		remap(4) // window / resource ID only
	case x11.OpcodeChangeProperty:
		remap(4) // window (property atom at [8:12] is NOT a resource ID)
	case x11.OpcodeCreatePixmap:
		remap(4) // pixmap ID
		remap(8) // drawable
	case x11.OpcodeFreePixmap:
		remap(4)
	case x11.OpcodeCreateGC:
		remap(4) // GC ID
		remap(8) // drawable
	case x11.OpcodeChangeGC, x11.OpcodeFreeGC:
		remap(4) // GC ID ([8:12] is value-mask, not a resource ID)
	case x11.OpcodeCopyGC:
		remap(4) // src GC
		remap(8) // dst GC
	case x11.OpcodeOpenFont, x11.OpcodeCloseFont:
		remap(4) // font ID
	case x11.OpcodeClearArea:
		remap(4) // window
	case x11.OpcodeCopyArea, x11.OpcodeCopyPlane:
		remap(4)  // src drawable
		remap(8)  // dst drawable
		remap(12) // GC
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
			remap(4)  // pid
			remap(8)  // drawable
		case x11.RenderChangePicture:
			remap(4)  // pid
		case x11.RenderFreePicture:
			remap(4)  // pid
		case x11.RenderComposite:
			remap(8)  // src picture
			remap(12) // mask picture (None=0 passes through safely)
			remap(16) // dst picture
		case x11.RenderTrapezoids, x11.RenderTriangles, x11.RenderTriStrip, x11.RenderTriFan:
			remap(8)  // src picture
			remap(12) // dst picture
		case x11.RenderSetPictureTransform:
			remap(4) // pic
		case x11.RenderSetPictureFilter:
			remap(4) // pic
		case x11.RenderCreateSolidFill, x11.RenderCreateLinearGradient,
			x11.RenderCreateRadialGradient, x11.RenderCreateConicalGradient:
			remap(4) // pid only; no drawable
		}
	default:
		// Draw commands and anything else: drawable at [4], GC at [8].
		remap(4)
		remap(8)
	}
	return out
}

// Client connects to a proxxxy-server and forwards X11 traffic to the local display.
type Client struct {
	serverAddr string

	server net.Conn
	srvW   sync.Mutex // serialises writes to server

	mu        sync.Mutex
	xConns    map[uint32]net.Conn
	decoders  map[uint32]*compress.Decoder
	idRemaps  map[uint32]idRemap    // resource ID remapping for synthesised connections
	synthDone map[uint32]chan struct{} // closed at SESSION_LIVE to switch synth relays to forward mode
}

func New(serverAddr string) *Client {
	return &Client{
		serverAddr: serverAddr,
		xConns:     make(map[uint32]net.Conn),
		decoders:   make(map[uint32]*compress.Decoder),
		idRemaps:   make(map[uint32]idRemap),
		synthDone:  make(map[uint32]chan struct{}),
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

	// SESSION_LIVE: switch synthesis relays to live-forward mode.
	// The synthesis X connections stay in xConns so live traffic continues through
	// them (they hold the synthesised resources). synthRelay goroutines switch from
	// drain-all to forward-replies-and-events mode.
	c.mu.Lock()
	for _, done := range c.synthDone {
		close(done)
	}
	c.synthDone = make(map[uint32]chan struct{})
	c.mu.Unlock()

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
	newBase, err := readAndConsumeSetupReply(xconn, setupBytes[0])
	if err != nil {
		log.Println("client: read X setup reply:", err)
		xconn.Close()
		return
	}

	var order binary.ByteOrder = binary.LittleEndian
	if setupBytes[0] == 0x42 {
		order = binary.BigEndian
	}

	c.mu.Lock()
	c.xConns[connID] = xconn
	if oldBase != 0 && newBase != 0 && oldBase != newBase {
		log.Printf("client: synthesis conn %d: oldBase=0x%08x mask=0x%08x newBase=0x%08x",
			connID, oldBase, oldMask, newBase)
		c.idRemaps[connID] = idRemap{oldBase, oldMask, newBase, order}
	} else {
		log.Printf("client: synthesis conn %d: oldBase=0x%08x newBase=0x%08x (no remap needed)",
			connID, oldBase, newBase)
	}
	// synthRelay runs immediately in drain mode, discarding synthesis-phase
	// events/errors whose sequence numbers would confuse the app's Xlib. Once
	// SESSION_LIVE is signalled via done, it switches to forwarding mode:
	// events (type ≥ 2) reach the app; errors (type 0) and replies (type 1)
	// are still discarded because their sequence numbers are synthesis-internal.
	done := make(chan struct{})
	c.synthDone[connID] = done
	c.mu.Unlock()
	go c.synthRelay(connID, xconn, done, order, appSeqNum)
}

// readAndConsumeSetupReply reads and discards one X11 server setup reply from
// conn, returning the new resource-id-base on success.
func readAndConsumeSetupReply(conn net.Conn, byteOrderByte byte) (uint32, error) {
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return 0, fmt.Errorf("read setup reply header: %w", err)
	}
	var order binary.ByteOrder = binary.LittleEndian
	if byteOrderByte == 0x42 {
		order = binary.BigEndian
	}
	if hdr[0] != 1 {
		// Failed or authenticate — read reason and discard.
		reasonLen := int(hdr[1])
		io.ReadFull(conn, make([]byte, reasonLen))
		return 0, fmt.Errorf("X11 setup rejected (code %d)", hdr[0])
	}
	dataLen := int(order.Uint16(hdr[6:8])) * 4
	data := make([]byte, dataLen)
	if _, err := io.ReadFull(conn, data); err != nil {
		return 0, fmt.Errorf("read setup reply data: %w", err)
	}
	if dataLen < 12 {
		return 0, fmt.Errorf("setup reply data too short")
	}
	// resource-id-base is at offset 4 in the additional data (byte 12 overall).
	return order.Uint32(data[4:8]), nil
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

	if !ok {
		xconn, err = dialX11(localDisplay())
		if err != nil {
			log.Println("client: dial local display:", err)
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
func (c *Client) synthRelay(connID uint32, xconn net.Conn, done <-chan struct{}, order binary.ByteOrder, appSeqNum uint32) {
	defer func() {
		xconn.Close()
		c.mu.Lock()
		delete(c.xConns, connID)
		delete(c.idRemaps, connID)
		c.mu.Unlock()
	}()

	hdr := make([]byte, 32)
	// readMsg reads one X11 message from xconn into hdr and returns any
	// variable-length tail (replies and GenericEvents only).
	readMsg := func() (msgType byte, tail []byte, err error) {
		if _, err = io.ReadFull(xconn, hdr); err != nil {
			return
		}
		msgType = hdr[0]
		if msgType == 1 || msgType == 35 { // reply or GenericEvent
			n := int(order.Uint32(hdr[4:8])) * 4
			if n > 0 {
				tail = make([]byte, n)
				_, err = io.ReadFull(xconn, tail)
			}
		}
		return
	}

	// Phase 1: drain until SESSION_LIVE.
	for {
		msgType, _, err := readMsg()
		if err != nil {
			return
		}
		if msgType == 0 {
			badID := order.Uint32(hdr[4:8])
			minor := order.Uint16(hdr[8:10])
			major := hdr[10]
			log.Printf("client: synthRelay conn %d: X error during synthesis: code=%d major=%d minor=%d badID=0x%08x",
				connID, hdr[1], major, minor, badID)
		}
		select {
		case <-done:
			// SESSION_LIVE received — move to barrier phase.
		default:
			continue
		}
		break
	}

	// Phase 2: barrier. Send GetInputFocus so the X server processes it after
	// all synthesis commands. Drain until we receive its reply, capturing the
	// reply's sequence number (N_synth) so we can compute the seq offset.
	barrierReq := [4]byte{x11.OpcodeGetInputFocus, 0}
	order.PutUint16(barrierReq[2:], 1) // length = 1 unit (4 bytes)
	if _, err := xconn.Write(barrierReq[:]); err != nil {
		log.Printf("client: synthRelay conn %d: write GetInputFocus: %v", connID, err)
		return
	}
	var nSynth uint16
	for {
		msgType, _, err := readMsg()
		if err != nil {
			return
		}
		if msgType == 0 {
			badID := order.Uint32(hdr[4:8])
			log.Printf("client: synthRelay conn %d: X error during barrier: code=%d major=%d badID=0x%08x",
				connID, hdr[1], hdr[10], badID)
		}
		if msgType == 1 {
			nSynth = order.Uint16(hdr[2:4])
			break
		}
	}

	// Phase 3: forward all messages back to the server with rewritten sequence
	// numbers. seqOffset maps synthesis-xconn seq space to app seq space so
	// Xlib on the app side recognises replies to its own requests.
	seqOffset := uint16(appSeqNum) - nSynth
	for {
		msgType, tail, err := readMsg()
		if err != nil {
			return
		}
		if msgType == 0 {
			badID := order.Uint32(hdr[4:8])
			log.Printf("client: synthRelay conn %d: X error during live: code=%d major=%d badID=0x%08x",
				connID, hdr[1], hdr[10], badID)
		}
		order.PutUint16(hdr[2:4], order.Uint16(hdr[2:4])+seqOffset)
		var full []byte
		if len(tail) > 0 {
			full = make([]byte, 32+len(tail))
			copy(full, hdr)
			copy(full[32:], tail)
		} else {
			full = make([]byte, 32)
			copy(full, hdr)
		}
		c.srvW.Lock()
		wire.WriteX11Data(c.server, connID, full) //nolint:errcheck
		c.srvW.Unlock()
	}
}

func (c *Client) relayXToServer(connID uint32, xconn net.Conn) {
	defer func() {
		xconn.Close()
		c.mu.Lock()
		delete(c.xConns, connID)
		delete(c.decoders, connID)
		delete(c.idRemaps, connID)
		c.mu.Unlock()
	}()
	buf := make([]byte, 32*1024)
	for {
		n, err := xconn.Read(buf)
		if n > 0 {
			c.srvW.Lock()
			werr := wire.WriteX11Data(c.server, connID, buf[:n])
			c.srvW.Unlock()
			if werr != nil {
				return
			}
		}
		if err != nil {
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
// Supports :N and :N.S (Unix socket). TCP displays (host:N) are not yet supported.
func dialX11(display string) (net.Conn, error) {
	if len(display) == 0 || display[0] != ':' {
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

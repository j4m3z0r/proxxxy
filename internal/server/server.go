package server

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"

	"james.id.au/proxxxy/internal/compress"
	"james.id.au/proxxxy/internal/wire"
	"james.id.au/proxxxy/internal/x11"
)

// Server presents a fake X display and relays to a connected proxxxy-client.
type Server struct {
	displayNum int
	tcpPort    int
	statsPort  int
	listenAddr string // host portion for TCP bind (default "127.0.0.1")

	unixL     net.Listener
	tcpL      net.Listener
	statsHTTP *http.Server

	mu             sync.Mutex
	clientConn     net.Conn   // current proxxxy-client (nil = none connected)
	synthActive    bool       // true while runSynthesis is in progress; blocks live relay
	clientW        sync.Mutex // serialises writes to clientConn
	nextID         atomic.Uint32
	appConns       map[uint32]net.Conn
	appState       map[uint32]*x11.AppConn
	encoders       sync.Map // uint32 connID → *compress.Encoder
}

func New(displayNum, tcpPort, statsPort int, listenAddr string) *Server {
	if listenAddr == "" {
		listenAddr = "127.0.0.1"
	}
	return &Server{
		displayNum: displayNum,
		tcpPort:    tcpPort,
		statsPort:  statsPort,
		listenAddr: listenAddr,
		appConns:   make(map[uint32]net.Conn),
		appState:   make(map[uint32]*x11.AppConn),
	}
}

func (s *Server) Start() error {
	socketPath := fmt.Sprintf("/tmp/.X11-unix/X%d", s.displayNum)
	lockPath := fmt.Sprintf("/tmp/.X%d-lock", s.displayNum)

	if err := os.MkdirAll("/tmp/.X11-unix", 0o1777); err != nil {
		return fmt.Errorf("mkdir X11-unix: %w", err)
	}
	os.Remove(lockPath)
	if err := os.WriteFile(lockPath, fmt.Appendf(nil, "%10d\n", os.Getpid()), 0o444); err != nil {
		return fmt.Errorf("write lock: %w", err)
	}
	os.Remove(socketPath)
	unixL, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen unix: %w", err)
	}
	if err = os.Chmod(socketPath, 0o777); err != nil {
		unixL.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}

	tcpL, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.listenAddr, s.tcpPort))
	if err != nil {
		unixL.Close()
		return fmt.Errorf("listen tcp: %w", err)
	}

	s.unixL = unixL
	s.tcpL = tcpL

	go s.acceptX11(unixL)
	go s.acceptClients(tcpL)
	s.startStatsHTTP()
	return nil
}

func (s *Server) Stop() {
	if s.statsHTTP != nil {
		s.statsHTTP.Close()
	}
	if s.unixL != nil {
		s.unixL.Close()
	}
	if s.tcpL != nil {
		s.tcpL.Close()
	}
	os.Remove(fmt.Sprintf("/tmp/.X11-unix/X%d", s.displayNum))
	os.Remove(fmt.Sprintf("/tmp/.X%d-lock", s.displayNum))
}

func (s *Server) startStatsHTTP() {
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", s.handleStats)
	s.statsHTTP = &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", s.statsPort),
		Handler: mux,
	}
	go func() {
		if err := s.statsHTTP.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Println("server: stats HTTP:", err)
		}
	}()
}

func (s *Server) acceptX11(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		id := s.nextID.Add(1)
		s.mu.Lock()
		s.appConns[id] = conn
		s.mu.Unlock()
		go s.relayAppToClient(id, conn)
	}
}

func (s *Server) relayAppToClient(connID uint32, app net.Conn) {
	defer func() {
		log.Printf("server: relayAppToClient conn %d exiting, closing app socket", connID)
		app.Close()
		s.mu.Lock()
		delete(s.appConns, connID)
		s.mu.Unlock()
	}()

	// Read connection setup, capture bytes, then forward synchronously.
	var setupBuf bytes.Buffer
	order, setupBytes, err := parseConnSetup(app, &setupBuf)
	if err != nil {
		return
	}
	ac := x11.NewAppConn(connID, order)
	ac.SetSetupReq(setupBytes)
	enc := compress.NewEncoder(connID, 4*1024*1024) // 4 MB dict per connection
	// Store appState before forwarding setup bytes so the setup reply that
	// arrives in readFromClient can find the AppConn and record ridBase.
	s.mu.Lock()
	s.appState[connID] = ac
	s.mu.Unlock()

	if setupBuf.Len() > 0 {
		s.sendToClient(connID, setupBuf.Bytes())
	}

	s.encoders.Store(connID, enc)
	defer func() {
		s.encoders.Delete(connID)
		s.mu.Lock()
		delete(s.appState, connID)
		s.mu.Unlock()
		enc.OnClientDisconnect()
	}()

	drainRequests(app, ac, enc, func(msgs []wire.Msg) {
		var out int64
		for _, m := range msgs {
			out += int64(len(m.Payload))
		}
		enc.Stats.BytesOut.Add(out)
		if !s.sendMsgsToClient(msgs) {
			// Dropped — no client or synthesis active. Send fake events so
			// app threads don't deadlock or hang:
			//   - OpcodeNeedsReply: fake zero-data success reply unblocks XLib
			//   - ShmPutImage+send_event: fake ShmCompletion unblocks SWGL
			// Fire during both full disconnect (c==nil) AND synthesis active,
			// since in both cases live requests are dropped and no real reply
			// will arrive from the X server.
			s.mu.Lock()
			shouldFake := s.clientConn == nil || s.synthActive
			s.mu.Unlock()
			if shouldFake {
				for _, m := range msgs {
					if m.Type != wire.MsgX11Data || len(m.Payload) < 5 {
						continue
					}
					opcode := m.Payload[4] // [conn_id:4][opcode:1][...]
					minor := byte(0)
					if len(m.Payload) >= 6 {
						minor = m.Payload[5]
					}
					// Core requests use OpcodeNeedsReply; extension requests
					// (>=128, e.g. XKB XkbGetMap) use the per-extension table keyed
					// on the major opcode learned from QueryExtension. Without this,
					// a synchronous extension request issued during the disconnect
					// window gets no reply and Xlib's _XReply blocks the whole app —
					// leaving its window black after reconnect.
					if x11.OpcodeNeedsReply(opcode) || (opcode >= 128 && ac.ExtNeedsReply(opcode, minor)) {
						seq := uint16(ac.SeqNum())
						log.Printf("server: conn %d: no client, fake reply opcode=%d minor=%d seq=%d",
							ac.ID, opcode, minor, seq)
						sendFakeX11Reply(app, seq, ac.Order)
					}
					// MIT-SHM ShmPutImage with send_event=True: send a fake
					// ShmCompletion so Firefox's SWGL compositor does not hang
					// waiting for an event that Xvfb will never generate (the
					// request was dropped because there is no client to relay it).
					if opcode == shmMajorOpcode && len(m.Payload) >= 44 &&
						m.Payload[5] == shmPutImageMinor && m.Payload[34] != 0 {
						drawable := ac.Order.Uint32(m.Payload[8:12])
						shmseg := ac.Order.Uint32(m.Payload[36:40])
						offset := ac.Order.Uint32(m.Payload[40:44])
						seq := uint16(ac.SeqNum())
						log.Printf("server: conn %d: no client, fake ShmCompletion seq=%d drawable=0x%x shmseg=0x%x",
							ac.ID, seq, drawable, shmseg)
						sendFakeShmCompletion(app, seq, drawable, shmseg, offset, ac.Order)
					}
				}
			}
		}
	})
}

// sendMsgsToClient forwards msgs to the current proxxxy-client if one is
// connected and synthesis is not active. Returns false if msgs were discarded.
func (s *Server) sendMsgsToClient(msgs []wire.Msg) bool {
	if len(msgs) == 0 {
		return true
	}
	s.mu.Lock()
	c := s.clientConn
	synth := s.synthActive
	s.mu.Unlock()
	if c == nil || synth {
		// No client, or synthesis in progress — live traffic discarded. Apps
		// will redraw after the Expose injection that follows SESSION_LIVE.
		return false
	}
	s.clientW.Lock()
	defer s.clientW.Unlock()
	for _, msg := range msgs {
		if err := wire.Write(c, msg.Type, msg.Payload); err != nil {
			log.Println("server: write to client:", err)
			return false
		}
	}
	return true
}

// shmMajorOpcode is the MIT-SHM extension opcode on Xvfb (always 130).
// shmEventBase is the first event code for MIT-SHM on Xvfb (always 65).
// shmPutImageMinor is the minor opcode for ShmPutImage within MIT-SHM.
// These values are stable for Xvfb; a future improvement would track them
// dynamically from QueryExtension replies.
const (
	shmMajorOpcode   = 130
	shmEventBase     = 65
	shmPutImageMinor = 3
)

// sendFakeShmCompletion writes a synthesized ShmCompletion event to app.
// Prevents Firefox's SWGL compositor from hanging when ShmPutImage is dropped.
func sendFakeShmCompletion(app net.Conn, seqNum uint16, drawable, shmseg, offset uint32, order binary.ByteOrder) {
	var evt [32]byte
	evt[0] = shmEventBase // ShmCompletion event type (firstEvent + 0)
	evt[1] = 0
	order.PutUint16(evt[2:4], seqNum)
	order.PutUint32(evt[4:8], drawable)
	order.PutUint16(evt[8:10], shmPutImageMinor) // minor_event = X_ShmPutImage
	evt[10] = shmMajorOpcode                     // major_event = SHM extension opcode
	evt[11] = 0
	order.PutUint32(evt[12:16], shmseg)
	order.PutUint32(evt[16:20], offset)
	// bytes 20-31: pad (zero)
	app.Write(evt[:]) //nolint:errcheck
}

// sendFakeX11Reply writes a zero-data X11 success reply to app with the given
// 16-bit sequence number. This unblocks an XLib thread waiting for a reply to
// a request sent during a client-disconnect window.
func sendFakeX11Reply(app net.Conn, seqNum uint16, order binary.ByteOrder) {
	var reply [32]byte
	reply[0] = 1 // reply indicator
	order.PutUint16(reply[2:4], seqNum)
	app.Write(reply[:]) //nolint:errcheck
}

func (s *Server) sendToClient(connID uint32, data []byte) {
	s.mu.Lock()
	c := s.clientConn
	s.mu.Unlock()
	if c == nil {
		return
	}
	s.clientW.Lock()
	defer s.clientW.Unlock()
	if err := wire.WriteX11Data(c, connID, data); err != nil {
		log.Println("server: write to client:", err)
	}
}

func (s *Server) acceptClients(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go s.handleClient(conn)
	}
}

func (s *Server) handleClient(conn net.Conn) {
	defer conn.Close()

	// Server sends HELLO first.
	if err := wire.WriteHello(conn); err != nil {
		return
	}
	msg, err := wire.Read(conn)
	if err != nil || msg.Type != wire.MsgHello {
		return
	}
	version, err := wire.ReadHello(msg.Payload)
	if err != nil || version != wire.ProtocolVersion {
		log.Printf("server: version mismatch (got %d, want %d)", version, wire.ProtocolVersion)
		return
	}

	// Block live relay while synthesis runs so that app requests don't race
	// with synthesis commands on the new X connection. Set clientConn so
	// synthesis can write to the client, but synthActive=true makes
	// sendMsgsToClient discard live traffic until synthesis is complete.
	s.mu.Lock()
	prev := s.clientConn
	s.clientConn = conn
	s.synthActive = true
	s.mu.Unlock()

	// Disconnect the previous client before synthesis begins. When it receives
	// a TCP close, its readFromClient loop exits and its relay goroutines drain.
	// Without this, the old client's synthRelay goroutines keep forwarding X
	// server events with stale sequence offsets, corrupting Xlib's seq counter.
	if prev != nil {
		prev.Close()
	}

	defer func() {
		s.mu.Lock()
		if s.clientConn == conn {
			s.clientConn = nil
		}
		s.mu.Unlock()
	}()
	defer func() {
		s.encoders.Range(func(_, val any) bool {
			val.(*compress.Encoder).OnClientDisconnect()
			return true
		})
	}()

	// Phase 2: synthesise existing X11 state for the reconnecting client.
	// synthActive is cleared inside runSynthesis before Expose injection.
	s.runSynthesis()

	// Unblock apps that connected before this client arrived and are still
	// waiting for their X11 setup reply.
	s.relayPendingSetups()

	log.Println("server: client connected")
	s.readFromClient(conn)
	log.Println("server: client disconnected")
}

func (s *Server) readFromClient(conn net.Conn) {
	for {
		msg, err := wire.Read(conn)
		if err != nil {
			return
		}
		if msg.Type != wire.MsgX11Data {
			continue
		}
		connID, data, err := wire.ParseX11Data(msg.Payload)
		if err != nil {
			continue
		}
		// Intercept first data from each new X connection to capture rid.
		s.mu.Lock()
		ac := s.appState[connID]
		app := s.appConns[connID]
		s.mu.Unlock()
		if ac != nil {
			if base, _ := ac.RID(); base == 0 {
				if b, m, ok := x11.ParseSetupReply(data, ac.Order); ok {
					ac.SetRID(b, m)
				}
			}
		}
		if app != nil {
			// Learn extension major opcodes from QueryExtension replies so the
			// disconnect-window fake-reply path knows which extension a dropped
			// request belongs to. A QueryExtension reply is [1][pad][seq:2]
			// [len:4=0][present:1][major:1][firstEvent:1][firstError:1][pad...].
			if len(data) >= 12 && data[0] == 1 {
				ac.LearnQueryExtensionReply(ac.Order.Uint16(data[2:4]), data[8] != 0, data[9])
			}
			if _, werr := app.Write(data); werr != nil {
				log.Println("server: write to app:", werr)
			}
		}
	}
}

// relayPendingSetups re-sends setup bytes for app connections that arrived
// before any proxxxy-client connected. Their setup bytes were dropped at the
// time (clientConn was nil); sending them now as MsgX11Data causes the client
// to open a fresh X connection, receive the real setup reply, and forward it
// back — unblocking the waiting app.
func (s *Server) relayPendingSetups() {
	s.mu.Lock()
	type item struct {
		connID uint32
		bytes  []byte
	}
	var pending []item
	for connID, ac := range s.appState {
		if base, _ := ac.RID(); base == 0 {
			if sb := ac.SetupReq(); len(sb) > 0 {
				pending = append(pending, item{connID, sb})
			}
		}
	}
	s.mu.Unlock()

	for _, p := range pending {
		log.Printf("server: re-relaying setup for pre-client conn %d", p.connID)
		s.sendToClient(p.connID, p.bytes)
	}
}

// Addr returns the TCP address the server is listening on (useful in tests).
func (s *Server) Addr() string {
	if s.tcpL == nil {
		return ""
	}
	return s.tcpL.Addr().String()
}

// WriteToClient is exported for use in synthesis (Phase 2).
func (s *Server) WriteToClient(msgType byte, payload []byte) error {
	s.mu.Lock()
	c := s.clientConn
	s.mu.Unlock()
	if c == nil {
		return io.ErrClosedPipe
	}
	s.clientW.Lock()
	defer s.clientW.Unlock()
	return wire.Write(c, msgType, payload)
}

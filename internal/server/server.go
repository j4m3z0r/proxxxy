package server

import (
	"bytes"
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

	unixL     net.Listener
	tcpL      net.Listener
	statsHTTP *http.Server

	mu         sync.Mutex
	clientConn net.Conn   // current proxxxy-client (nil = none connected)
	clientW    sync.Mutex // serialises writes to clientConn
	nextID     atomic.Uint32
	appConns   map[uint32]net.Conn
	appState   map[uint32]*x11.AppConn
	encoders   sync.Map // uint32 connID → *compress.Encoder
}

func New(displayNum, tcpPort, statsPort int) *Server {
	return &Server{
		displayNum: displayNum,
		tcpPort:    tcpPort,
		statsPort:  statsPort,
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

	tcpL, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.tcpPort))
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
		app.Close()
		s.mu.Lock()
		delete(s.appConns, connID)
		s.mu.Unlock()
	}()

	// Read connection setup, capture bytes, then forward synchronously.
	var setupBuf bytes.Buffer
	order, _, err := parseConnSetup(app, &setupBuf)
	if err != nil {
		return
	}
	if setupBuf.Len() > 0 {
		s.sendToClient(connID, setupBuf.Bytes())
	}

	ac := x11.NewAppConn(connID, order)
	enc := compress.NewEncoder(connID, 4*1024*1024) // 4 MB dict per connection
	s.mu.Lock()
	s.appState[connID] = ac
	s.mu.Unlock()

	s.encoders.Store(connID, enc)
	defer func() {
		s.encoders.Delete(connID)
		s.mu.Lock()
		delete(s.appState, connID)
		s.mu.Unlock()
		enc.OnClientDisconnect()
	}()

	drainRequests(app, ac, enc, func(data []byte) {
		enc.Stats.BytesIn.Add(int64(len(data)))
		s.sendToClient(connID, data)
		enc.Stats.BytesOut.Add(int64(len(data)))
	})
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

	// Set clientConn before announcing live, so data isn't dropped.
	s.mu.Lock()
	s.clientConn = conn
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.clientConn == conn {
			s.clientConn = nil
		}
		s.mu.Unlock()
	}()

	// Phase 2: synthesise existing X11 state for the reconnecting client.
	s.runSynthesis()

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
		s.mu.Lock()
		app := s.appConns[connID]
		s.mu.Unlock()
		if app != nil {
			if _, werr := app.Write(data); werr != nil {
				log.Println("server: write to app:", werr)
			}
		}
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

package server

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"james.id.au/proxxxy/internal/wire"
	"james.id.au/proxxxy/internal/x11"
)

// Server presents a fake X display and relays to a connected proxxxy-client.
type Server struct {
	displayNum int
	tcpPort    int

	unixL net.Listener
	tcpL  net.Listener

	mu         sync.Mutex
	clientConn net.Conn   // current proxxxy-client (nil = none connected)
	clientW    sync.Mutex // serialises writes to clientConn
	nextID     atomic.Uint32
	appConns   map[uint32]net.Conn
	appState   map[uint32]*x11.AppConn
}

func New(displayNum, tcpPort int) *Server {
	return &Server{
		displayNum: displayNum,
		tcpPort:    tcpPort,
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
	return nil
}

func (s *Server) Stop() {
	if s.unixL != nil {
		s.unixL.Close()
	}
	if s.tcpL != nil {
		s.tcpL.Close()
	}
	os.Remove(fmt.Sprintf("/tmp/.X11-unix/X%d", s.displayNum))
	os.Remove(fmt.Sprintf("/tmp/.X%d-lock", s.displayNum))
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
	s.mu.Lock()
	s.appState[connID] = ac
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.appState, connID)
		s.mu.Unlock()
	}()

	drainRequests(app, ac, func(b []byte) {
		s.sendToClient(connID, b)
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

	// Phase 1: no state to replay.
	wire.Write(conn, wire.MsgSessionResume, nil)
	wire.Write(conn, wire.MsgSessionLive, nil)

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

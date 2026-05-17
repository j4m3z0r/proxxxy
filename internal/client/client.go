package client

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"sync"

	"james.id.au/proxxxy/internal/compress"
	"james.id.au/proxxxy/internal/wire"
)

// Client connects to a proxxxy-server and forwards X11 traffic to the local display.
type Client struct {
	serverAddr string

	server net.Conn
	srvW   sync.Mutex // serialises writes to server

	mu       sync.Mutex
	xConns   map[uint32]net.Conn
	decoders map[uint32]*compress.Decoder
}

func New(serverAddr string) *Client {
	return &Client{
		serverAddr: serverAddr,
		xConns:     make(map[uint32]net.Conn),
		decoders:   make(map[uint32]*compress.Decoder),
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

	// Consume SESSION_RESUME … SESSION_LIVE (Phase 1: these carry no state).
	for {
		msg, err = wire.Read(conn)
		if err != nil {
			return fmt.Errorf("read session init: %w", err)
		}
		if msg.Type == wire.MsgSessionLive {
			break
		}
		switch msg.Type {
		case wire.MsgX11Data:
			c.handleX11Data(msg.Payload)
		case wire.MsgDictDefine, wire.MsgDictRef, wire.MsgDictExpire, wire.MsgTemplateDefine, wire.MsgTemplateApply:
			c.handleCompressed(msg)
		}
	}

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

func (c *Client) handleX11Data(payload []byte) {
	connID, data, err := wire.ParseX11Data(payload)
	if err != nil {
		log.Println("client: parse X11Data:", err)
		return
	}

	c.mu.Lock()
	xconn, ok := c.xConns[connID]
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
	c.mu.Unlock()

	if !ok {
		var err error
		xconn, err = dialX11(localDisplay())
		if err != nil {
			log.Println("client: dial local display:", err)
			return
		}
		c.mu.Lock()
		c.xConns[connID] = xconn
		c.mu.Unlock()
		go c.relayXToServer(connID, xconn)
	}

	if _, err := xconn.Write(data); err != nil {
		log.Println("client: write to display:", err)
	}
}

func (c *Client) relayXToServer(connID uint32, xconn net.Conn) {
	defer func() {
		xconn.Close()
		c.mu.Lock()
		delete(c.xConns, connID)
		delete(c.decoders, connID)
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

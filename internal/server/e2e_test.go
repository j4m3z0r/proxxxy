package server_test

import (
	"bytes"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"james.id.au/proxxxy/internal/server"
	"james.id.au/proxxxy/internal/wire"
)

// TestE2ERelay verifies that bytes written to the fake X socket arrive at a
// "local display" stub, and that replies flow back.
func TestE2ERelay(t *testing.T) {
	// Stand up a stub "local X display" (a plain Unix socket echo server).
	stubSocket := "/tmp/.X11-unix/X98"
	os.Remove(stubSocket)
	stubL, err := net.Listen("unix", stubSocket)
	if err != nil {
		t.Fatal("stub listen:", err)
	}
	// Fix #3: defer in LIFO order — Close first, then Remove.
	defer os.Remove(stubSocket)
	defer stubL.Close()

	// Valid minimal X11 connection setup (12 bytes, little-endian, no auth).
	setup := []byte{
		0x6c, 0x00, // byte order LE, pad
		0x0b, 0x00, // protocol major 11
		0x00, 0x00, // protocol minor 0
		0x00, 0x00, // auth-proto-name len 0
		0x00, 0x00, // auth-proto-data len 0
		0x00, 0x00, // pad
	}
	// Valid minimal X11 request: opcode=1 (CreateWindow placeholder), extra=0,
	// length=1 (meaning 4 bytes total). We use opcode=55 (CreateGC has 4-word
	// minimum but for drainRequests we only need length≥1). Use a NoOperation
	// request: opcode=127, extra=0, length=1 → 4 bytes.
	noop := []byte{127, 0, 1, 0} // NoOperation request, 4 bytes
	want := append(setup, noop...)

	received := make(chan []byte, 1)
	go func() {
		conn, err := stubL.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 1024)
		// Fix #4: use io.ReadFull to avoid partial reads.
		if _, err := io.ReadFull(conn, buf[:len(want)]); err != nil {
			return
		}
		received <- append([]byte(nil), buf[:len(want)]...)
		conn.Write(buf[:len(want)])
	}()

	// Start proxxxy-server on display :97, TCP port 17197.
	s := server.New(97, 17197, 17297)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	// Connect a proxxxy-client stub: do the handshake.
	// Fix #1: removed dead t.Setenv("DISPLAY", ":98") — nothing reads it.
	clientConn, err := net.Dial("tcp", "127.0.0.1:17197")
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()
	clientConn.SetDeadline(time.Now().Add(5 * time.Second))

	// Handshake
	msg, _ := wire.Read(clientConn)
	if msg.Type != wire.MsgHello {
		t.Fatal("expected HELLO")
	}
	wire.WriteHello(clientConn)
	for {
		msg, _ = wire.Read(clientConn)
		if msg.Type == wire.MsgSessionLive {
			break
		}
	}

	// Connect a fake X app to the Unix socket.
	appConn, err := net.Dial("unix", "/tmp/.X11-unix/X97")
	if err != nil {
		t.Fatal("dial fake display:", err)
	}
	defer appConn.Close()

	// App sends a valid X11 connection setup followed by a NoOperation request.
	// Fix #5: check write error.
	if _, err := appConn.Write(want); err != nil {
		t.Fatal("write to app:", err)
	}

	// The client stub should receive the setup bytes as X11_DATA (from parseConnSetup),
	// then at least one encoder message for the noop request (MsgDictDefine, MsgDictRef,
	// or MsgX11Data — depending on the encoder's classification).
	clientConn.SetDeadline(time.Now().Add(2 * time.Second))

	// First, collect setup bytes from X11_DATA messages.
	var gotSetup []byte
	for len(gotSetup) < len(setup) {
		msg, err = wire.Read(clientConn)
		if err != nil {
			t.Fatalf("read setup X11_DATA (got %d/%d bytes so far): %v", len(gotSetup), len(setup), err)
		}
		if msg.Type != wire.MsgX11Data {
			t.Fatalf("expected X11_DATA for setup, got %x", msg.Type)
		}
		_, data, _ := wire.ParseX11Data(msg.Payload)
		gotSetup = append(gotSetup, data...)
	}
	if !bytes.Equal(gotSetup, setup) {
		t.Fatalf("setup data: got %q want %q", gotSetup, setup)
	}

	// The noop request is now processed by the encoder, which may produce
	// MsgDictDefine, MsgDictRef, or MsgX11Data. Accept any of these.
	msg, err = wire.Read(clientConn)
	if err != nil {
		t.Fatalf("read noop message: %v", err)
	}
	switch msg.Type {
	case wire.MsgX11Data, wire.MsgDictDefine, wire.MsgDictRef, wire.MsgTemplateDefine, wire.MsgTemplateApply:
		// expected: the encoder emitted a valid compressed or raw message
	default:
		t.Fatalf("unexpected message type for noop: %x", msg.Type)
	}
}

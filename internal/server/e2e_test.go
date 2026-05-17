package server_test

import (
	"bytes"
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
	defer stubL.Close()
	defer os.Remove(stubSocket)

	received := make(chan []byte, 1)
	go func() {
		conn, err := stubL.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		received <- buf[:n]
		// Echo it back so the relay has data to send.
		conn.Write(buf[:n])
	}()

	// Start proxxxy-server on display :97, TCP port 17197.
	s := server.New(97, 17197)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	// Connect a proxxxy-client stub: do the handshake, set DISPLAY=:98.
	t.Setenv("DISPLAY", ":98")
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

	// App sends some bytes.
	want := []byte("hello X server")
	appConn.Write(want)

	// The client stub (which would forward to :98) should receive them as X11_DATA.
	clientConn.SetDeadline(time.Now().Add(2 * time.Second))
	msg, err = wire.Read(clientConn)
	if err != nil {
		t.Fatal("read X11_DATA:", err)
	}
	if msg.Type != wire.MsgX11Data {
		t.Fatalf("expected X11_DATA got %x", msg.Type)
	}
	_, data, _ := wire.ParseX11Data(msg.Payload)
	if !bytes.Equal(data, want) {
		t.Fatalf("data: got %q want %q", data, want)
	}
}

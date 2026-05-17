package server_test

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"james.id.au/proxxxy/internal/server"
	"james.id.au/proxxxy/internal/wire"
)

func TestHandshakeVersionMismatch(t *testing.T) {
	s := server.New(99, 17199, 17299)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	conn, err := net.Dial("tcp", "127.0.0.1:17199")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))

	// Server sends HELLO first
	msg, err := wire.Read(conn)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Type != wire.MsgHello {
		t.Fatalf("expected HELLO got %x", msg.Type)
	}

	// Send HELLO with wrong version
	var p [4]byte
	binary.LittleEndian.PutUint32(p[:], 999)
	wire.Write(conn, wire.MsgHello, p[:])

	// Server should close the connection
	conn.SetDeadline(time.Now().Add(time.Second))
	_, err = wire.Read(conn)
	if err == nil {
		t.Fatal("expected connection close after version mismatch")
	}
}

func TestHandshakeSuccess(t *testing.T) {
	s := server.New(99, 17200, 17300)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	conn, err := net.Dial("tcp", "127.0.0.1:17200")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))

	// Read server HELLO
	msg, err := wire.Read(conn)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Type != wire.MsgHello {
		t.Fatalf("expected HELLO got %x", msg.Type)
	}

	// Send matching HELLO
	wire.WriteHello(conn)

	// Expect SESSION_RESUME
	msg, err = wire.Read(conn)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Type != wire.MsgSessionResume {
		t.Fatalf("expected SESSION_RESUME got %x", msg.Type)
	}

	// Expect SESSION_LIVE
	msg, err = wire.Read(conn)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Type != wire.MsgSessionLive {
		t.Fatalf("expected SESSION_LIVE got %x", msg.Type)
	}
}

package wire_test

import (
	"bytes"
	"testing"

	"james.id.au/proxxxy/internal/wire"
)

func TestRoundTripEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := wire.Write(&buf, wire.MsgSessionResume, nil); err != nil {
		t.Fatal(err)
	}
	msg, err := wire.Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Type != wire.MsgSessionResume {
		t.Fatalf("type: got %x want %x", msg.Type, wire.MsgSessionResume)
	}
	if len(msg.Payload) != 0 {
		t.Fatalf("expected empty payload, got %d bytes", len(msg.Payload))
	}
}

func TestHelloRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := wire.WriteHello(&buf); err != nil {
		t.Fatal(err)
	}
	msg, err := wire.Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Type != wire.MsgHello {
		t.Fatalf("type: got %x want %x", msg.Type, wire.MsgHello)
	}
	v, err := wire.ReadHello(msg.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if v != wire.ProtocolVersion {
		t.Fatalf("version: got %d want %d", v, wire.ProtocolVersion)
	}
}

func TestX11DataRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte{0xde, 0xad, 0xbe, 0xef}
	if err := wire.WriteX11Data(&buf, 7, payload); err != nil {
		t.Fatal(err)
	}
	msg, err := wire.Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	connID, data, err := wire.ParseX11Data(msg.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if connID != 7 {
		t.Fatalf("connID: got %d want 7", connID)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("data: got %v want %v", data, payload)
	}
}

func TestOversizedMessageRejected(t *testing.T) {
	// Manually craft a message claiming 128 MB payload
	buf := bytes.NewBuffer([]byte{wire.MsgX11Data, 0x00, 0x00, 0x00, 0x08})
	_, err := wire.Read(buf)
	if err == nil {
		t.Fatal("expected error for oversized message")
	}
}

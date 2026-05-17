package x11_test

import (
	"encoding/binary"
	"testing"

	"james.id.au/proxxxy/internal/x11"
)

func TestParseRequestHeader(t *testing.T) {
	// CreateWindow request: opcode=1, length=8 (32 bytes)
	var buf [4]byte
	buf[0] = 1    // opcode
	buf[1] = 24   // depth (extra byte)
	binary.LittleEndian.PutUint16(buf[2:], 8) // length in 4-byte units
	hdr, err := x11.ParseRequestHeader(buf[:])
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Opcode != 1 {
		t.Fatalf("opcode: got %d want 1", hdr.Opcode)
	}
	if hdr.ByteLen != 32 {
		t.Fatalf("byteLen: got %d want 32", hdr.ByteLen)
	}
}

func TestParseRequestHeaderBigEndian(t *testing.T) {
	var buf [4]byte
	buf[0] = 2  // ChangeWindowAttributes
	buf[1] = 0
	binary.BigEndian.PutUint16(buf[2:], 3) // length = 12 bytes
	hdr, err := x11.ParseRequestHeaderOrder(buf[:], binary.BigEndian)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.ByteLen != 12 {
		t.Fatalf("byteLen: got %d want 12", hdr.ByteLen)
	}
}

func TestSetupByteOrder(t *testing.T) {
	// Little-endian connection setup starts with 0x6c ('l')
	order, err := x11.ParseByteOrder([]byte{0x6c})
	if err != nil {
		t.Fatal(err)
	}
	if order != binary.LittleEndian {
		t.Fatal("expected little endian")
	}

	// Big-endian starts with 0x42 ('B')
	order, err = x11.ParseByteOrder([]byte{0x42})
	if err != nil {
		t.Fatal(err)
	}
	if order != binary.BigEndian {
		t.Fatal("expected big endian")
	}
}

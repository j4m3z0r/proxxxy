package x11

import (
	"encoding/binary"
	"fmt"
)

// ByteOrder wraps binary.ByteOrder.
type ByteOrder = binary.ByteOrder

// RequestHeader is the decoded 4-byte prefix of every X11 request.
type RequestHeader struct {
	Opcode  byte
	Extra   byte   // meaning is opcode-specific
	ByteLen uint32 // total request length in bytes (length field × 4)
}

// ParseByteOrder reads the first byte of an X11 connection setup to determine
// byte order. 0x6c = little-endian, 0x42 = big-endian.
func ParseByteOrder(b []byte) (ByteOrder, error) {
	if len(b) < 1 {
		return nil, fmt.Errorf("x11: empty byte-order indicator")
	}
	switch b[0] {
	case 0x6c:
		return binary.LittleEndian, nil
	case 0x42:
		return binary.BigEndian, nil
	default:
		return nil, fmt.Errorf("x11: unknown byte-order byte 0x%02x", b[0])
	}
}

// ParseRequestHeader parses a 4-byte X11 request header using little-endian byte order.
func ParseRequestHeader(b []byte) (RequestHeader, error) {
	return ParseRequestHeaderOrder(b, binary.LittleEndian)
}

// ParseRequestHeaderOrder parses a 4-byte X11 request header using the given byte order.
func ParseRequestHeaderOrder(b []byte, order ByteOrder) (RequestHeader, error) {
	if len(b) < 4 {
		return RequestHeader{}, fmt.Errorf("x11: request header too short (%d bytes)", len(b))
	}
	length := order.Uint16(b[2:4])
	if length == 0 {
		return RequestHeader{}, fmt.Errorf("x11: request length field is zero")
	}
	return RequestHeader{
		Opcode:  b[0],
		Extra:   b[1],
		ByteLen: uint32(length) * 4,
	}, nil
}

// U32 reads a uint32 at offset off from b using the given byte order.
func U32(b []byte, off int, order ByteOrder) uint32 {
	return order.Uint32(b[off : off+4])
}

// U16 reads a uint16 at offset off from b using the given byte order.
func U16(b []byte, off int, order ByteOrder) uint16 {
	return order.Uint16(b[off : off+2])
}

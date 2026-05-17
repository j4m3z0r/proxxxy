package wire

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	ProtocolVersion uint32 = 2

	MsgHello          byte = 0x01
	MsgSessionResume  byte = 0x02
	MsgSessionLive    byte = 0x03
	MsgX11Data        byte = 0x04
	MsgDictDefine     byte = 0x10
	MsgDictRef        byte = 0x11
	MsgDictExpire     byte = 0x12
	MsgTemplateDefine byte = 0x13
	MsgTemplateApply  byte = 0x14
	MsgRegionAck      byte = 0x20

	maxMessageSize = 64 * 1024 * 1024 // 64 MB
)

// Msg is a decoded proxxxy wire message.
type Msg struct {
	Type    byte
	Payload []byte
}

// Write sends one message to w. payload may be nil for empty messages.
func Write(w io.Writer, msgType byte, payload []byte) error {
	if uint32(len(payload)) > maxMessageSize {
		return fmt.Errorf("wire: payload too large: %d bytes", len(payload))
	}
	hdr := [5]byte{msgType}
	binary.LittleEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := w.Write(payload)
		return err
	}
	return nil
}

// Read reads one message from r.
func Read(r io.Reader) (Msg, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Msg{}, err
	}
	length := binary.LittleEndian.Uint32(hdr[1:])
	if length > maxMessageSize {
		return Msg{}, fmt.Errorf("wire: message too large: %d bytes", length)
	}
	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return Msg{}, err
		}
	}
	return Msg{Type: hdr[0], Payload: payload}, nil
}

// WriteHello sends a HELLO message containing ProtocolVersion.
func WriteHello(w io.Writer) error {
	var p [4]byte
	binary.LittleEndian.PutUint32(p[:], ProtocolVersion)
	return Write(w, MsgHello, p[:])
}

// ReadHello parses a HELLO payload and returns the version field.
func ReadHello(payload []byte) (uint32, error) {
	if len(payload) < 4 {
		return 0, fmt.Errorf("wire: HELLO payload too short (%d bytes)", len(payload))
	}
	return binary.LittleEndian.Uint32(payload[:4]), nil
}

// WriteX11Data sends an X11_DATA message: [conn_id:4 LE][raw x11 bytes...].
func WriteX11Data(w io.Writer, connID uint32, data []byte) error {
	p := make([]byte, 4+len(data))
	binary.LittleEndian.PutUint32(p[:4], connID)
	copy(p[4:], data)
	return Write(w, MsgX11Data, p)
}

// ParseX11Data splits an X11_DATA payload into connID and raw X11 bytes.
func ParseX11Data(payload []byte) (connID uint32, data []byte, err error) {
	if len(payload) < 4 {
		return 0, nil, fmt.Errorf("wire: X11_DATA payload too short (%d bytes)", len(payload))
	}
	return binary.LittleEndian.Uint32(payload[:4]), payload[4:], nil
}

// ParseCompressedMsg splits a compressed message payload (MsgDictDefine,
// MsgDictRef, MsgDictExpire, MsgTemplateDefine, MsgTemplateApply) into
// connID and the remainder of the payload.
func ParseCompressedMsg(payload []byte) (connID uint32, rest []byte, err error) {
	if len(payload) < 4 {
		return 0, nil, fmt.Errorf("wire: compressed msg payload too short (%d bytes)", len(payload))
	}
	return binary.LittleEndian.Uint32(payload[:4]), payload[4:], nil
}

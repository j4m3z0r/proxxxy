package server

import (
	"bufio"
	"encoding/binary"
	"io"
	"log"
	"net"

	"james.id.au/proxxxy/internal/compress"
	"james.id.au/proxxxy/internal/wire"
	"james.id.au/proxxxy/internal/x11"
)

// parseConnSetup reads the connection setup request from an X11 app connection
// and returns the byte order. The setup bytes are forwarded to out.
func parseConnSetup(conn net.Conn, out io.Writer) (binary.ByteOrder, []byte, error) {
	// Minimum setup request is 12 bytes.
	hdr := make([]byte, 12)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return nil, nil, err
	}
	order, err := x11.ParseByteOrder(hdr[:1])
	if err != nil {
		return nil, nil, err
	}
	authNameLen := int(order.Uint16(hdr[6:8]))
	authDataLen := int(order.Uint16(hdr[8:10]))
	// Pad to 4-byte boundary.
	pad := func(n int) int { return (4 - n%4) % 4 }
	extra := pad(authNameLen) + authNameLen + pad(authDataLen) + authDataLen
	rest := make([]byte, extra)
	if _, err := io.ReadFull(conn, rest); err != nil {
		return nil, nil, err
	}
	full := append(hdr, rest...)
	if out != nil {
		out.Write(full)
	}
	return order, full, nil
}

// drainRequests reads X11 requests from app, updates appConn state, encodes them
// via enc, and forwards the resulting messages via sendMsgFn. Runs until the
// connection is closed.
func drainRequests(app net.Conn, ac *x11.AppConn, enc *compress.Encoder, sendMsgFn func(wire.Msg)) {
	r := bufio.NewReaderSize(app, 32*1024)
	hdr := make([]byte, 4)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			return
		}
		reqHdr, err := x11.ParseRequestHeaderOrder(hdr, ac.Order)
		if err != nil {
			log.Println("x11 parse:", err)
			return
		}
		body := make([]byte, reqHdr.ByteLen-4)
		if _, err := io.ReadFull(r, body); err != nil {
			return
		}
		full := make([]byte, reqHdr.ByteLen)
		copy(full, hdr)
		copy(full[4:], body)
		ac.ProcessRequest(full)

		// Drain any DICT_EXPIRE messages.
		for _, msg := range enc.DrainExpiredDicts() {
			sendMsgFn(msg)
		}
		// Encode and send compressed messages.
		for _, msg := range enc.Encode(0, full, ac.Order) {
			sendMsgFn(msg)
		}
	}
}

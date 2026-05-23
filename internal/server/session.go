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

// drainRequests reads X11 requests from app, compresses them via enc, and
// forwards the resulting wire messages via sendFn. Runs until the connection
// is closed.
func drainRequests(app net.Conn, ac *x11.AppConn, enc *compress.Encoder, sendFn func([]wire.Msg)) {
	r := bufio.NewReaderSize(app, 32*1024)
	hdr := make([]byte, 4)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			log.Printf("server: drainRequests conn %d read hdr error: %v", ac.ID, err)
			return
		}

		var full []byte
		stdLen := ac.Order.Uint16(hdr[2:4])
		if stdLen == 0 {
			// BigRequest extension: length==0 in the standard 2-byte field means
			// the next 4 bytes hold the actual request length in 4-byte units.
			// Total request size = extLen*4 bytes (includes both header segments).
			var extBuf [4]byte
			if _, err := io.ReadFull(r, extBuf[:]); err != nil {
				return
			}
			extLen := ac.Order.Uint32(extBuf[:])
			if extLen < 2 {
				log.Printf("server: drainRequests conn %d: invalid BigRequest length %d hdr=%x", ac.ID, extLen, hdr)
				return
			}
			total := extLen * 4 // total bytes including 4-byte std hdr + 4-byte ext hdr
			full = make([]byte, total)
			copy(full[0:4], hdr)
			copy(full[4:8], extBuf[:])
			if total > 8 {
				if _, err := io.ReadFull(r, full[8:]); err != nil {
					return
				}
			}
		} else {
			byteLen := uint32(stdLen) * 4
			if byteLen < 4 {
				log.Printf("server: drainRequests conn %d: invalid request length %d hdr=%x", ac.ID, stdLen, hdr)
				return
			}
			full = make([]byte, byteLen)
			copy(full, hdr)
			if byteLen > 4 {
				if _, err := io.ReadFull(r, full[4:]); err != nil {
					return
				}
			}
		}

		ac.ProcessRequest(full)
		enc.Stats.BytesIn.Add(int64(len(full)))
		// Phase 3 encoder bypassed: client-side decoder is not yet production-
		// ready (dict inClient state pollutes across reconnects). Forward raw
		// X11 bytes as MsgX11Data until Phase 3 is fully stabilised.
		p := make([]byte, 4+len(full))
		binary.LittleEndian.PutUint32(p[:4], ac.ID)
		copy(p[4:], full)
		sendFn([]wire.Msg{{Type: wire.MsgX11Data, Payload: p}})
	}
}

package client

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"james.id.au/proxxxy/internal/wire"
	"james.id.au/proxxxy/internal/x11"
)

// buildQueryExtension constructs a little-endian QueryExtension request.
func buildQueryExtension(name string) []byte {
	nameLen := len(name)
	pad := (4 - nameLen%4) % 4
	totalLen := 4 + 2 + 2 + nameLen + pad
	buf := make([]byte, totalLen)
	buf[0] = x11.OpcodeQueryExtension
	buf[1] = 0
	binary.LittleEndian.PutUint16(buf[2:4], uint16(totalLen/4))
	binary.LittleEndian.PutUint16(buf[4:6], uint16(nameLen))
	copy(buf[8:], name)
	return buf
}

// TestGPUExtensionSuppression verifies that QueryExtension replies for DRI2,
// DRI3, and Present are rewritten to "not present" before reaching the app,
// while replies for other extensions pass through unchanged.
func TestGPUExtensionSuppression(t *testing.T) {
	tests := []struct {
		extName        string
		wantSuppressed bool
	}{
		{"DRI3", true},
		{"DRI2", true},
		{"Present", true},
		{"RENDER", false},
		{"MIT-SHM", false},
	}

	for _, tc := range tests {
		t.Run(tc.extName, func(t *testing.T) {
			// serverA/serverB: simulate the TCP connection to proxxxy-server.
			// xconnA/xconnB: simulate the Unix socket to the real X server.
			//   - Client writes requests to xconnA; xconnB.Read() receives them.
			//   - Real X server writes replies to xconnB; xconnA.Read() receives them.
			serverA, serverB := net.Pipe()
			xconnA, xconnB := net.Pipe()
			defer serverA.Close()
			defer serverB.Close()
			defer xconnA.Close()
			defer xconnB.Close()

			c := New("unused")
			c.server = serverA

			const connID = uint32(1)
			c.mu.Lock()
			c.xConns[connID] = xconnA
			c.connOrders[connID] = binary.LittleEndian
			c.connSeqNums[connID] = 0
			c.mu.Unlock()

			// Start the relay goroutine: reads replies from xconnA, writes to serverA.
			go c.relayXToServer(connID, xconnA)

			// Build the QueryExtension request payload (prefixed with connID).
			req := buildQueryExtension(tc.extName)
			payload := make([]byte, 4+len(req))
			binary.LittleEndian.PutUint32(payload[:4], connID)
			copy(payload[4:], req)

			// handleX11Data writes to xconnA which blocks until xconnB reads.
			// Run it in a goroutine and signal when done so we can safely write
			// the reply after connSuppressed is populated.
			done := make(chan struct{})
			go func() {
				defer close(done)
				c.handleX11Data(payload)
			}()

			// Consume the request from xconnB (unblocks handleX11Data's Write).
			xconnB.SetDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
			reqBuf := make([]byte, len(req))
			if _, err := io.ReadFull(xconnB, reqBuf); err != nil {
				t.Fatalf("read QueryExtension request from xconnB: %v", err)
			}

			// Wait for handleX11Data to finish so connSuppressed is populated
			// before we write the reply (which triggers relayXToServer to check it).
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("handleX11Data did not complete")
			}

			// Verify seqnum was incremented to 1.
			c.mu.Lock()
			seqnum := uint16(c.connSeqNums[connID])
			c.mu.Unlock()
			if seqnum != 1 {
				t.Fatalf("expected seqnum=1, got %d", seqnum)
			}

			// Write a fake "present=1, major-opcode=149" reply into xconnB.
			// relayXToServer reads it from xconnA and should suppress it for GPU extensions.
			var reply [32]byte
			reply[0] = 1 // reply discriminator
			binary.LittleEndian.PutUint16(reply[2:4], seqnum)
			reply[8] = 1   // present = true
			reply[9] = 149 // major-opcode
			if _, err := xconnB.Write(reply[:]); err != nil {
				t.Fatalf("write fake reply to xconnB: %v", err)
			}

			// Read the (potentially rewritten) reply forwarded to the server.
			serverB.SetDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
			msg, err := wire.Read(serverB)
			if err != nil {
				t.Fatalf("read wire message from serverB: %v", err)
			}
			if msg.Type != wire.MsgX11Data {
				t.Fatalf("expected MsgX11Data, got 0x%02x", msg.Type)
			}
			_, forwarded, err := wire.ParseX11Data(msg.Payload)
			if err != nil {
				t.Fatalf("parse X11Data: %v", err)
			}
			if len(forwarded) < 12 {
				t.Fatalf("forwarded reply too short: %d bytes", len(forwarded))
			}

			present := forwarded[8]
			majorOpcode := forwarded[9]

			if tc.wantSuppressed {
				if present != 0 {
					t.Errorf("%s: expected present=0 (suppressed), got %d", tc.extName, present)
				}
				if majorOpcode != 0 {
					t.Errorf("%s: expected major-opcode=0 (suppressed), got %d", tc.extName, majorOpcode)
				}
			} else {
				if present != 1 {
					t.Errorf("%s: expected present=1 (pass-through), got %d", tc.extName, present)
				}
				if majorOpcode != 149 {
					t.Errorf("%s: expected major-opcode=149 (pass-through), got %d", tc.extName, majorOpcode)
				}
			}
		})
	}
}

// xlog is a man-in-the-middle X11 traffic logger.
//
// It listens on a fake X display, forwards connections to a real backend
// display, and emits one JSON line per X11 message to stdout. The tool sits
// idle until the first client connects, captures traffic for -n seconds, then
// exits.
//
// Usage:
//
//	xlog [-display N] [-forward :M] [-n seconds]
//
// Example: log 5 seconds of xterm traffic, forwarding to display :1
//
//	DISPLAY=:1 xlog -display 3 -n 5
//	xterm &   # with DISPLAY=:3
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"james.id.au/proxxxy/internal/x11"
)

var (
	capStart = time.Now()
	enc      = json.NewEncoder(os.Stdout)
)

type entry struct {
	T    float64 `json:"t"`
	Dir  string  `json:"dir"`
	Type string  `json:"type,omitempty"`
	Op   int     `json:"op,omitempty"`
	Name string  `json:"name,omitempty"`
	Len  int     `json:"len,omitempty"`
	Seq  int     `json:"seq,omitempty"`
	Code int     `json:"code,omitempty"`
}

func emit(e entry) {
	e.T = time.Since(capStart).Seconds()
	enc.Encode(e) //nolint:errcheck
}

var opNames = map[byte]string{
	x11.OpcodeCreateWindow:           "CreateWindow",
	x11.OpcodeChangeWindowAttributes: "ChangeWindowAttributes",
	x11.OpcodeGetWindowAttributes:    "GetWindowAttributes",
	x11.OpcodeDestroyWindow:          "DestroyWindow",
	x11.OpcodeDestroySubwindows:      "DestroySubwindows",
	x11.OpcodeChangeSaveSet:          "ChangeSaveSet",
	x11.OpcodeReparentWindow:         "ReparentWindow",
	x11.OpcodeMapWindow:              "MapWindow",
	x11.OpcodeMapSubwindows:          "MapSubwindows",
	x11.OpcodeUnmapWindow:            "UnmapWindow",
	x11.OpcodeUnmapSubwindows:        "UnmapSubwindows",
	x11.OpcodeConfigureWindow:        "ConfigureWindow",
	x11.OpcodeCirculateWindow:        "CirculateWindow",
	x11.OpcodeGetGeometry:            "GetGeometry",
	x11.OpcodeQueryTree:              "QueryTree",
	x11.OpcodeInternAtom:             "InternAtom",
	x11.OpcodeGetAtomName:            "GetAtomName",
	x11.OpcodeChangeProperty:         "ChangeProperty",
	x11.OpcodeDeleteProperty:         "DeleteProperty",
	x11.OpcodeGetProperty:            "GetProperty",
	x11.OpcodeCreatePixmap:           "CreatePixmap",
	x11.OpcodeFreePixmap:             "FreePixmap",
	x11.OpcodeCreateGC:               "CreateGC",
	x11.OpcodeChangeGC:               "ChangeGC",
	x11.OpcodeCopyGC:                 "CopyGC",
	x11.OpcodeSetDashes:              "SetDashes",
	x11.OpcodeSetClipRectangles:      "SetClipRectangles",
	x11.OpcodeFreeGC:                 "FreeGC",
	x11.OpcodeClearArea:              "ClearArea",
	x11.OpcodeCopyArea:               "CopyArea",
	x11.OpcodeCopyPlane:              "CopyPlane",
	x11.OpcodePolyPoint:              "PolyPoint",
	x11.OpcodePolyLine:               "PolyLine",
	x11.OpcodePolySegment:            "PolySegment",
	x11.OpcodePolyRectangle:          "PolyRectangle",
	x11.OpcodePolyArc:                "PolyArc",
	x11.OpcodeFillPoly:               "FillPoly",
	x11.OpcodePolyFillRectangle:      "PolyFillRectangle",
	x11.OpcodePolyFillArc:            "PolyFillArc",
	x11.OpcodePutImage:               "PutImage",
	x11.OpcodeGetImage:               "GetImage",
	x11.OpcodePolyText8:              "PolyText8",
	x11.OpcodePolyText16:             "PolyText16",
	x11.OpcodeImageText8:             "ImageText8",
	x11.OpcodeImageText16:            "ImageText16",
	x11.OpcodeCreateColormap:         "CreateColormap",
	x11.OpcodeFreeColormap:           "FreeColormap",
	x11.OpcodeCopyColormapAndFree:    "CopyColormapAndFree",
	x11.OpcodeInstallColormap:        "InstallColormap",
	x11.OpcodeUninstallColormap:      "UninstallColormap",
	x11.OpcodeAllocColor:             "AllocColor",
	x11.OpcodeAllocNamedColor:        "AllocNamedColor",
	x11.OpcodeLookupColor:            "LookupColor",
	x11.OpcodeFreeColors:             "FreeColors",
	x11.OpcodeOpenFont:               "OpenFont",
	x11.OpcodeCloseFont:              "CloseFont",
	x11.OpcodeQueryFont:              "QueryFont",
	x11.OpcodeSetInputFocus:          "SetInputFocus",
	x11.OpcodeGetInputFocus:          "GetInputFocus",
	x11.OpcodeQueryExtension:         "QueryExtension",
	x11.OpcodeListExtensions:         "ListExtensions",
	x11.OpcodeChangeKeyboardMapping:  "ChangeKeyboardMapping",
	x11.OpcodeBell:                   "Bell",
	x11.OpcodeChangePointerControl:   "ChangePointerControl",
	x11.OpcodeSetScreenSaver:         "SetScreenSaver",
	x11.OpcodeForceScreenSaver:       "ForceScreenSaver",
}

func opName(op byte) string {
	if n, ok := opNames[op]; ok {
		return n
	}
	return fmt.Sprintf("op%d", int(op))
}

var eventNames = map[byte]string{
	2:  "KeyPress",
	3:  "KeyRelease",
	4:  "ButtonPress",
	5:  "ButtonRelease",
	6:  "MotionNotify",
	7:  "EnterNotify",
	8:  "LeaveNotify",
	9:  "FocusIn",
	10: "FocusOut",
	11: "KeymapNotify",
	12: "Expose",
	13: "GraphicsExposure",
	14: "NoExposure",
	15: "VisibilityNotify",
	16: "CreateNotify",
	17: "DestroyNotify",
	18: "UnmapNotify",
	19: "MapNotify",
	20: "MapRequest",
	21: "ReparentNotify",
	22: "ConfigureNotify",
	23: "ConfigureRequest",
	24: "GravityNotify",
	25: "ResizeRequest",
	26: "CirculateNotify",
	27: "CirculateRequest",
	28: "PropertyNotify",
	29: "SelectionClear",
	30: "SelectionRequest",
	31: "SelectionNotify",
	32: "ColormapNotify",
	33: "ClientMessage",
	34: "MappingNotify",
	35: "GenericEvent",
}

func eventName(code byte) string {
	code &= 0x7f // strip send-event bit
	if n, ok := eventNames[code]; ok {
		return n
	}
	return fmt.Sprintf("event%d", int(code))
}

var errorNames = map[byte]string{
	1:  "BadRequest",
	2:  "BadValue",
	3:  "BadWindow",
	4:  "BadPixmap",
	5:  "BadAtom",
	6:  "BadCursor",
	7:  "BadFont",
	8:  "BadMatch",
	9:  "BadDrawable",
	10: "BadAccess",
	11: "BadAlloc",
	12: "BadColor",
	13: "BadGC",
	14: "BadIDChoice",
	15: "BadName",
	16: "BadLength",
	17: "BadImplementation",
}

func errorName(code byte) string {
	if n, ok := errorNames[code]; ok {
		return n
	}
	return fmt.Sprintf("error%d", int(code))
}

func socketPath(display string) string {
	n := strings.TrimPrefix(display, ":")
	if dot := strings.IndexByte(n, '.'); dot >= 0 {
		n = n[:dot]
	}
	return filepath.Join("/tmp/.X11-unix", "X"+n)
}

func readSetupReq(r io.Reader) ([]byte, binary.ByteOrder, error) {
	hdr := make([]byte, 12)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, nil, err
	}
	var order binary.ByteOrder
	switch hdr[0] {
	case 0x6c:
		order = binary.LittleEndian
	case 0x42:
		order = binary.BigEndian
	default:
		return nil, nil, fmt.Errorf("unknown byte-order byte 0x%02x", hdr[0])
	}
	pad4 := func(n int) int { return (n + 3) &^ 3 }
	authNameLen := int(order.Uint16(hdr[6:8]))
	authDataLen := int(order.Uint16(hdr[8:10]))
	rest := make([]byte, pad4(authNameLen)+pad4(authDataLen))
	if len(rest) > 0 {
		if _, err := io.ReadFull(r, rest); err != nil {
			return nil, nil, err
		}
	}
	buf := make([]byte, len(hdr)+len(rest))
	copy(buf, hdr)
	copy(buf[len(hdr):], rest)
	return buf, order, nil
}

func readSetupReply(r io.Reader, order binary.ByteOrder) ([]byte, error) {
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	addLen := int(order.Uint16(hdr[6:8])) * 4
	rest := make([]byte, addLen)
	if len(rest) > 0 {
		if _, err := io.ReadFull(r, rest); err != nil {
			return nil, err
		}
	}
	buf := make([]byte, len(hdr)+len(rest))
	copy(buf, hdr)
	copy(buf[len(hdr):], rest)
	return buf, nil
}

func handleConn(client net.Conn, backendSocket string, deadline time.Time) {
	defer client.Close()

	setupReq, order, err := readSetupReq(client)
	if err != nil {
		log.Printf("xlog: read setup: %v", err)
		return
	}

	backend, err := net.Dial("unix", backendSocket)
	if err != nil {
		log.Printf("xlog: connect backend %s: %v", backendSocket, err)
		return
	}
	defer backend.Close()

	if _, err := backend.Write(setupReq); err != nil {
		log.Printf("xlog: write setup to backend: %v", err)
		return
	}

	setupReply, err := readSetupReply(backend, order)
	if err != nil {
		log.Printf("xlog: read setup reply: %v", err)
		return
	}

	if _, err := client.Write(setupReply); err != nil {
		log.Printf("xlog: write setup reply to client: %v", err)
		return
	}

	emit(entry{Dir: "C2S", Type: "setup", Len: len(setupReq)})

	// Set deadline: client reads stop after the capture window.
	client.SetDeadline(deadline) //nolint:errcheck

	// Relay S2C (backend → client) in background, logging each message.
	go func() {
		hdr := make([]byte, 32)
		for {
			if _, err := io.ReadFull(backend, hdr); err != nil {
				return
			}
			disc := hdr[0]
			seq := int(order.Uint16(hdr[2:4]))
			var tail []byte
			if disc == 1 || disc == 35 { // reply or GenericEvent has variable tail
				n := int(order.Uint32(hdr[4:8])) * 4
				if n > 0 {
					tail = make([]byte, n)
					if _, err := io.ReadFull(backend, tail); err != nil {
						return
					}
				}
			}
			switch disc {
			case 0:
				emit(entry{Dir: "S2C", Type: "error", Code: int(hdr[1]), Name: errorName(hdr[1]), Seq: seq})
			case 1:
				emit(entry{Dir: "S2C", Type: "reply", Seq: seq, Len: 32 + len(tail)})
			default:
				emit(entry{Dir: "S2C", Type: "event", Code: int(disc & 0x7f), Name: eventName(disc)})
			}
			full := make([]byte, 32+len(tail))
			copy(full, hdr)
			copy(full[32:], tail)
			client.Write(full) //nolint:errcheck
		}
	}()

	// Relay C2S (client → backend), logging each request.
	var rhdr [4]byte
	for {
		if _, err := io.ReadFull(client, rhdr[:]); err != nil {
			return
		}
		h, err := x11.ParseRequestHeaderOrder(rhdr[:], order)
		if err != nil {
			return
		}
		body := make([]byte, h.ByteLen-4)
		if len(body) > 0 {
			if _, err := io.ReadFull(client, body); err != nil {
				return
			}
		}
		emit(entry{Dir: "C2S", Op: int(h.Opcode), Name: opName(h.Opcode), Len: int(h.ByteLen)})
		full := make([]byte, h.ByteLen)
		copy(full, rhdr[:])
		copy(full[4:], body)
		if _, err := backend.Write(full); err != nil {
			return
		}
	}
}

func main() {
	displayFlag := flag.String("display", "3", "display number to listen on (e.g. 3 for :3)")
	forwardFlag := flag.String("forward", "", "backend X display to forward to (default: $DISPLAY)")
	nFlag := flag.Int("n", 10, "seconds of traffic to capture after first connection")
	flag.Parse()

	listenPath := socketPath(*displayFlag)
	os.Remove(listenPath)

	backend := *forwardFlag
	if backend == "" {
		backend = os.Getenv("DISPLAY")
	}
	if backend == "" {
		log.Fatal("xlog: specify -forward or set $DISPLAY")
	}
	backendPath := socketPath(backend)

	ln, err := net.Listen("unix", listenPath)
	if err != nil {
		log.Fatalf("xlog: listen %s: %v", listenPath, err)
	}
	defer ln.Close()
	log.Printf("xlog: :%s → %s, capturing %ds from first connection", *displayFlag, backend, *nFlag)

	conn, err := ln.Accept()
	if err != nil {
		log.Fatalf("xlog: accept: %v", err)
	}
	capStart = time.Now()
	deadline := capStart.Add(time.Duration(*nFlag) * time.Second)
	log.Printf("xlog: first connection, t=0")

	handleConn(conn, backendPath, deadline)
	log.Printf("xlog: capture complete")
}

// Command inputtest injects synthetic keystrokes into an X11 window via the
// XTEST extension. It is a test helper for verifying the proxxxy input path
// end-to-end: keystrokes faked here go XQuartz -> proxxxy-client -> server ->
// app, and the app's echo travels back, so a resulting window-content change
// proves the full round trip.
//
// Usage: inputtest -win 0x40004f -text "echo hi" [-enter]
// If -win is omitted, the largest mapped child of the root is used.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/BurntSushi/xgb"
	"github.com/BurntSushi/xgb/xproto"
)

// dialX11 mirrors the proxxxy client: XQuartz sets DISPLAY to a socket path.
func dialX11(display string) (net.Conn, error) {
	if len(display) == 0 {
		return nil, fmt.Errorf("empty DISPLAY")
	}
	if display[0] == '/' {
		return net.Dial("unix", display)
	}
	if display[0] != ':' {
		return nil, fmt.Errorf("unsupported display format: %q", display)
	}
	num := display[1:]
	for i, ch := range num {
		if ch == '.' {
			num = num[:i]
			break
		}
	}
	return net.Dial("unix", fmt.Sprintf("/tmp/.X11-unix/X%s", num))
}

var pressOnly bool

func main() {
	winFlag := flag.String("win", "", "target window id (hex, e.g. 0x40004f); default = largest mapped root child")
	text := flag.String("text", "", "ASCII text to type")
	enter := flag.Bool("enter", false, "press Return after the text")
	resize := flag.String("resize", "", "resize the window to WxH (then back) to force a repaint (debug)")
	dumpExt := flag.Bool("dumpext", false, "dump extension major opcodes")
	click := flag.String("click", "", "click button 1 at window-relative X,Y (e.g. 30,15)")
	flag.BoolVar(&pressOnly, "press-only", false, "send KeyPress without KeyRelease (debug)")
	flag.Parse()

	display := os.Getenv("DISPLAY")
	conn, err := dialX11(display)
	if err != nil {
		log.Fatalf("dial %q: %v", display, err)
	}
	c, err := xgb.NewConnNet(conn)
	if err != nil {
		log.Fatalf("xgb NewConnNet: %v", err)
	}
	defer c.Close()
	// XQuartz does not advertise XTEST, so we use core-X11 XSendEvent. Apps must
	// be launched with allowSendEvents:true to honour synthetic events (xterm:
	// -xrm 'XTerm*allowSendEvents:true'). This still exercises the proxxxy event
	// relay path: injector -> XQuartz -> proxxxy-client -> server -> app.
	setup := xproto.Setup(c)
	root := setup.DefaultScreen(c).Root

	var win xproto.Window
	if *winFlag != "" {
		var id uint32
		fmt.Sscanf(*winFlag, "0x%x", &id)
		win = xproto.Window(id)
	} else {
		win = largestChild(c, root)
		if win == 0 {
			log.Fatal("no suitable child window found")
		}
		log.Printf("auto-selected window 0x%x", win)
	}

	if *dumpExt {
		for _, name := range []string{"RENDER", "SYNC", "DAMAGE", "XFIXES", "MIT-SHM",
			"XInputExtension", "XKEYBOARD", "Present", "DOUBLE-BUFFER", "SHAPE",
			"RANDR", "XINERAMA", "X-Resource", "BIG-REQUESTS", "GLX", "DRI2", "DRI3"} {
			r, err := xproto.QueryExtension(c, uint16(len(name)), name).Reply()
			if err != nil || r == nil || !r.Present {
				continue
			}
			log.Printf("ext %-18s major=%d firstEvent=%d firstError=%d", name, r.MajorOpcode, r.FirstEvent, r.FirstError)
		}
		return
	}

	if *click != "" {
		var x, y int
		fmt.Sscanf(*click, "%d,%d", &x, &y)
		// Translate to root coords for RootX/RootY (GTK menu positioning).
		rx, ry := int16(x), int16(y)
		if t, err := xproto.TranslateCoordinates(c, win, root, int16(x), int16(y)).Reply(); err == nil && t != nil {
			rx, ry = int16(t.DstX), int16(t.DstY)
		}
		log.Printf("clicking button 1 at win 0x%x (%d,%d) root (%d,%d)", win, x, y, rx, ry)
		// Move pointer there first (some widgets track motion/enter before press).
		xproto.WarpPointer(c, 0, win, 0, 0, 0, 0, int16(x), int16(y))
		sendButton(c, root, win, 4, 1, 0, int16(x), int16(y), rx, ry)                    // ButtonPress
		time.Sleep(60 * time.Millisecond)
		sendButton(c, root, win, 5, 1, uint16(xproto.ButtonMask1), int16(x), int16(y), rx, ry) // ButtonRelease
		sync(c)
		log.Printf("click done")
		return
	}

	if *resize != "" {
		var w, h uint32
		fmt.Sscanf(*resize, "%dx%d", &w, &h)
		g, err := xproto.GetGeometry(c, xproto.Drawable(win)).Reply()
		if err != nil {
			log.Fatalf("GetGeometry: %v", err)
		}
		log.Printf("resizing 0x%x from %dx%d to %dx%d (then back)", win, g.Width, g.Height, w, h)
		mask := uint16(xproto.ConfigWindowWidth | xproto.ConfigWindowHeight)
		xproto.ConfigureWindow(c, win, mask, []uint32{w, h})
		sync(c)
		time.Sleep(500 * time.Millisecond)
		xproto.ConfigureWindow(c, win, mask, []uint32{uint32(g.Width), uint32(g.Height)})
		sync(c)
		log.Printf("resize done")
		return
	}

	// Build keysym -> keycode map from the server's keyboard mapping.
	kc := buildKeycodeMap(c, setup)
	log.Printf("keycodes: shift=%d enter=%d", kc.shift, kc.enter)

	for _, ch := range *text {
		code, shift, ok := kc.lookup(ch)
		if !ok {
			log.Printf("no keycode for %q (0x%x), skipping", ch, ch)
			continue
		}
		log.Printf("char %q -> keycode %d shift=%v", ch, code, shift)
		state := uint16(0)
		if shift {
			state = uint16(xproto.KeyButMaskShift)
		}
		tap(c, root, win, code, state)
		time.Sleep(15 * time.Millisecond)
	}
	if *enter {
		tap(c, root, win, kc.enter, 0)
	}
	sync(c)
	log.Printf("done")
}

// tap sends a synthetic KeyPress then KeyRelease to win via XSendEvent.
func tap(c *xgb.Conn, root, win xproto.Window, code byte, state uint16) {
	press := xproto.KeyPressEvent{
		Detail: xproto.Keycode(code), Time: xproto.TimeCurrentTime,
		Root: root, Event: win, Child: 0,
		RootX: 1, RootY: 1, EventX: 1, EventY: 1,
		State: state, SameScreen: true,
	}
	xproto.SendEvent(c, false, win, uint32(xproto.EventMaskKeyPress), string(press.Bytes()))
	if !pressOnly {
		// xgb's KeyReleaseEvent.Bytes() delegates to KeyPressEvent.Bytes(), so it
		// emits event code 2 (KeyPress) instead of 3 (KeyRelease) — which xterm
		// echoes as a second character. Override byte[0] to the KeyRelease code.
		rel := press.Bytes()
		rel[0] = xproto.KeyRelease
		xproto.SendEvent(c, false, win, uint32(xproto.EventMaskKeyRelease), string(rel))
	}
}

// sendButton sends a synthetic ButtonPress (code 4) or ButtonRelease (code 5)
// via XSendEvent. Like KeyRelease, xgb's ButtonReleaseEvent.Bytes() emits the
// ButtonPress code, so for release we serialize a press and override byte[0].
func sendButton(c *xgb.Conn, root, win xproto.Window, code, button byte, state uint16, ex, ey, rx, ry int16) {
	ev := xproto.ButtonPressEvent{
		Detail: xproto.Button(button), Time: xproto.TimeCurrentTime,
		Root: root, Event: win, Child: 0,
		RootX: rx, RootY: ry, EventX: ex, EventY: ey,
		State: state, SameScreen: true,
	}
	b := ev.Bytes()
	b[0] = code
	mask := uint32(xproto.EventMaskButtonPress)
	if code == 5 {
		mask = uint32(xproto.EventMaskButtonRelease)
	}
	xproto.SendEvent(c, false, win, mask, string(b))
}

func sync(c *xgb.Conn) { xproto.GetInputFocus(c).Reply() }

func largestChild(c *xgb.Conn, root xproto.Window) xproto.Window {
	tree, err := xproto.QueryTree(c, root).Reply()
	if err != nil {
		return 0
	}
	var best xproto.Window
	var bestArea int
	for _, w := range tree.Children {
		attr, err := xproto.GetWindowAttributes(c, w).Reply()
		if err != nil || attr.MapState != xproto.MapStateViewable {
			continue
		}
		g, err := xproto.GetGeometry(c, xproto.Drawable(w)).Reply()
		if err != nil {
			continue
		}
		area := int(g.Width) * int(g.Height)
		if area > bestArea {
			bestArea, best = area, w
		}
	}
	return best
}

type keymap struct {
	byKeysym     map[uint32]byte
	shift, enter byte
}

func (k *keymap) lookup(ch rune) (code byte, shift bool, ok bool) {
	if kc, found := k.byKeysym[uint32(ch)]; found {
		return kc, false, true
	}
	// Try shifted form for uppercase / symbols already encoded as their keysym.
	return 0, false, false
}

func buildKeycodeMap(c *xgb.Conn, setup *xproto.SetupInfo) *keymap {
	min := setup.MinKeycode
	count := byte(setup.MaxKeycode - setup.MinKeycode + 1)
	reply, err := xproto.GetKeyboardMapping(c, min, count).Reply()
	if err != nil {
		log.Fatalf("GetKeyboardMapping: %v", err)
	}
	per := int(reply.KeysymsPerKeycode)
	km := &keymap{byKeysym: map[uint32]byte{}}
	for i := 0; i < int(count); i++ {
		code := byte(int(min) + i)
		for j := 0; j < per; j++ {
			ks := uint32(reply.Keysyms[i*per+j])
			if ks == 0 {
				continue
			}
			if _, exists := km.byKeysym[ks]; !exists {
				km.byKeysym[ks] = code
			}
		}
	}
	km.shift = km.byKeysym[0xffe1] // Shift_L
	km.enter = km.byKeysym[0xff0d] // Return
	return km
}

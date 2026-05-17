package main

import (
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/BurntSushi/xgb"
	"github.com/BurntSushi/xgb/xproto"
)

const (
	winW  uint16 = 300
	winH  uint16 = 300
	rectX int16  = 50
	rectY int16  = 50
	rectW uint16 = 200
	rectH uint16 = 200
)

func main() {
	X, err := xgb.NewConn()
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer X.Close()

	setup := xproto.Setup(X)
	screen := setup.DefaultScreen(X)

	wid, err := xproto.NewWindowId(X)
	if err != nil {
		fmt.Fprintln(os.Stderr, "window id:", err)
		os.Exit(1)
	}
	if err := xproto.CreateWindowChecked(X,
		screen.RootDepth, wid, screen.Root,
		0, 0, winW, winH, 1,
		xproto.WindowClassInputOutput, screen.RootVisual,
		xproto.CwBackPixel|xproto.CwEventMask,
		[]uint32{
			screen.BlackPixel,
			xproto.EventMaskExposure | xproto.EventMaskStructureNotify,
		},
	).Check(); err != nil {
		fmt.Fprintln(os.Stderr, "create window:", err)
		os.Exit(1)
	}

	// WM_DELETE_WINDOW
	wmProto, err := xproto.InternAtom(X, true, uint16(len("WM_PROTOCOLS")), "WM_PROTOCOLS").Reply()
	if err != nil {
		fmt.Fprintln(os.Stderr, "intern WM_PROTOCOLS:", err)
		os.Exit(1)
	}
	wmDel, err := xproto.InternAtom(X, true, uint16(len("WM_DELETE_WINDOW")), "WM_DELETE_WINDOW").Reply()
	if err != nil {
		fmt.Fprintln(os.Stderr, "intern WM_DELETE_WINDOW:", err)
		os.Exit(1)
	}
	delBytes := make([]byte, 4)
	xgb.Put32(delBytes, uint32(wmDel.Atom))
	xproto.ChangeProperty(X, xproto.PropModeReplace, wid,
		wmProto.Atom, xproto.AtomAtom, 32, 1, delBytes)

	title := "proxxxy testclient"
	xproto.ChangeProperty(X, xproto.PropModeReplace, wid,
		xproto.AtomWmName, xproto.AtomString, 8,
		uint32(len(title)), []byte(title))

	gcid, err := xproto.NewGcontextId(X)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gc id:", err)
		os.Exit(1)
	}
	if err := xproto.CreateGCChecked(X, gcid, xproto.Drawable(wid),
		xproto.GcForeground, []uint32{screen.WhitePixel},
	).Check(); err != nil {
		fmt.Fprintln(os.Stderr, "create gc:", err)
		os.Exit(1)
	}

	xproto.MapWindow(X, wid)

	pixels := [2]uint32{screen.WhitePixel, screen.BlackPixel}
	var idx atomic.Int32

	draw := func() {
		xproto.ChangeGC(X, gcid, xproto.GcForeground, []uint32{pixels[idx.Load()]})
		xproto.PolyFillRectangle(X, xproto.Drawable(wid), gcid,
			[]xproto.Rectangle{{X: rectX, Y: rectY, Width: rectW, Height: rectH}})
	}

	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for range t.C {
			idx.Store((idx.Load() + 1) % 2)
			draw()
		}
	}()

	deleteAtom := wmDel.Atom
	for {
		ev, xerr := X.WaitForEvent()
		if xerr != nil || ev == nil {
			return
		}
		switch e := ev.(type) {
		case xproto.ExposeEvent:
			draw()
		case xproto.ClientMessageEvent:
			if xproto.Atom(e.Data.Data32[0]) == deleteAtom {
				return
			}
		case xproto.DestroyNotifyEvent:
			return
		}
	}
}

package x11

// Core X11 request opcodes (X11 protocol specification §2).
const (
	OpcodeCreateWindow           = 1
	OpcodeChangeWindowAttributes = 2
	OpcodeGetWindowAttributes    = 3
	OpcodeDestroyWindow          = 4
	OpcodeDestroySubwindows      = 5
	OpcodeChangeSaveSet          = 6
	OpcodeReparentWindow         = 7
	OpcodeMapWindow              = 8
	OpcodeMapSubwindows          = 9
	OpcodeUnmapWindow            = 10
	OpcodeUnmapSubwindows        = 11
	OpcodeConfigureWindow        = 12
	OpcodeCirculateWindow        = 13
	OpcodeGetGeometry            = 14
	OpcodeQueryTree              = 15
	OpcodeInternAtom             = 16
	OpcodeGetAtomName            = 17
	OpcodeChangeProperty         = 18
	OpcodeDeleteProperty         = 19
	OpcodeGetProperty            = 20
	OpcodeCreatePixmap           = 53
	OpcodeFreePixmap             = 54
	OpcodeCreateGC               = 55
	OpcodeChangeGC               = 56
	OpcodeCopyGC                 = 57
	OpcodeSetDashes              = 58
	OpcodeSetClipRectangles      = 59
	OpcodeFreeGC                 = 60
	OpcodeClearArea              = 61
	OpcodeCopyArea               = 62
	OpcodeCopyPlane              = 63
	OpcodePolyPoint              = 64
	OpcodePolyLine               = 65
	OpcodePolySegment            = 66
	OpcodePolyRectangle          = 67
	OpcodePolyArc                = 68
	OpcodeFillPoly               = 69
	OpcodePolyFillRectangle      = 70
	OpcodePolyFillArc            = 71
	OpcodePutImage               = 72
	OpcodeGetImage               = 73
	OpcodePolyText8              = 74
	OpcodePolyText16             = 75
	OpcodeImageText8             = 76
	OpcodeImageText16            = 77
	OpcodeCreateColormap         = 78
	OpcodeFreeColormap           = 79
	OpcodeCopyColormapAndFree    = 80
	OpcodeInstallColormap        = 81
	OpcodeUninstallColormap      = 82
	OpcodeAllocColor             = 84
	OpcodeAllocNamedColor        = 85
	OpcodeLookupColor            = 92
	OpcodeFreeColors             = 88
	OpcodeCreateCursor           = 93
	OpcodeCreateGlyphCursor      = 94
	OpcodeFreeCursor             = 95
	OpcodeRecolorCursor          = 96
	OpcodeOpenFont               = 45
	OpcodeCloseFont              = 46
	OpcodeQueryFont              = 47
	OpcodeSetInputFocus          = 42
	OpcodeGetInputFocus          = 43
	OpcodeQueryExtension         = 98
	OpcodeListExtensions         = 99
	OpcodeChangeKeyboardMapping  = 100
	OpcodeBell                   = 104
	OpcodeChangePointerControl   = 105
	OpcodeSetScreenSaver         = 107
	OpcodeForceScreenSaver       = 115
)

// Reply/event/error discriminators.
const (
	ReplyDiscriminator = 1
	ErrorDiscriminator = 0
)

// OpcodeRender is the major opcode assigned to the RENDER extension by X.org.
// This value is conventional rather than guaranteed — it is the opcode in
// practice on virtually all X.org servers, but strictly speaking it is
// assigned at server start-up and could differ. We use it to track Picture
// objects so that synthesis can recreate them after reconnect.
const OpcodeRender = 139

// OpcodeMITSHM is the major opcode assigned to the MIT-SHM extension by X.org.
// Like OpcodeRender, this is conventional (X.org assigns it consistently) but
// not guaranteed. We use it to track ShmAttach calls so synthesis can replay
// them on the new connection, allowing ShmPutImage to work after reconnect.
const OpcodeMITSHM = 130

// MIT-SHM minor opcodes (shm.h).
const (
	SHMAttach       = 1
	SHMDetach       = 2
	SHMCreatePixmap = 5
)

// OpcodeSYNC is the major opcode assigned to the SYNC extension by X.org.
// We use it to track counter creation/destruction so synthesis can recreate
// counters, allowing SetCounter to work after reconnect (e.g., for WM sync).
const OpcodeSYNC = 134

// SYNC minor opcodes (syncproto.h).
const (
	SYNCCreateCounter  = 2
	SYNCDestroyCounter = 6
)

// OpcodeXInput is the major opcode assigned to the XInput extension by X.org.
// We use it to track XISelectEvents calls so synthesis can replay XI2 event
// masks after reconnect — without this, GTK3 apps receive no button/key events
// after reconnect because they use XISelectEvents (not XSelectInput) for input.
const OpcodeXInput = 131

// XI minor opcodes (XI2proto.h).
const (
	XISelectEvents = 46
)

// RENDER extension minor opcodes (renderproto.h, RENDER protocol v0.11+).
const (
	RenderCreatePicture             = 4
	RenderChangePicture             = 5
	RenderSetPictureClipRectangles  = 6
	RenderFreePicture               = 7
	RenderComposite                 = 8
	RenderTrapezoids                = 10
	RenderTriangles                 = 11
	RenderTriStrip                  = 12
	RenderTriFan                    = 13
	RenderCreateGlyphSet            = 16
	RenderReferenceGlyphSet         = 17
	RenderFreeGlyphSet              = 18
	RenderAddGlyphs                 = 20
	RenderFreeGlyphs                = 22
	RenderCompositeGlyphs8          = 23
	RenderCompositeGlyphs16         = 24
	RenderCompositeGlyphs32         = 25
	RenderFillRectangles            = 26
	RenderCreateCursor              = 27
	RenderSetPictureTransform       = 28
	RenderSetPictureFilter          = 30
	// Picture constructors without a backing drawable (RENDER protocol v0.7+).
	RenderCreateSolidFill       = 33
	RenderCreateLinearGradient  = 34
	RenderCreateRadialGradient  = 35
	RenderCreateConicalGradient = 36
)

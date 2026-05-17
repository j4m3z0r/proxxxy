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

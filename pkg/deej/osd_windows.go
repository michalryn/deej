package deej

import (
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"unsafe"

	"go.uber.org/zap"
)

// volumeOSD draws a small, click-through, always-on-top volume indicator near the bottom
// of the primary monitor - similar to the Windows volume flyout - whenever a slider moves.
// it runs its own Win32 message loop on a dedicated OS thread.

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	shcore   = syscall.NewLazyDLL("shcore.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procRegisterClassExW   = user32.NewProc("RegisterClassExW")
	procCreateWindowExW    = user32.NewProc("CreateWindowExW")
	procDefWindowProcW     = user32.NewProc("DefWindowProcW")
	procGetMessageW        = user32.NewProc("GetMessageW")
	procTranslateMessage   = user32.NewProc("TranslateMessage")
	procDispatchMessageW   = user32.NewProc("DispatchMessageW")
	procPostMessageW       = user32.NewProc("PostMessageW")
	procSetWindowPos       = user32.NewProc("SetWindowPos")
	procShowWindow         = user32.NewProc("ShowWindow")
	procSetTimer           = user32.NewProc("SetTimer")
	procKillTimer          = user32.NewProc("KillTimer")
	procInvalidateRect     = user32.NewProc("InvalidateRect")
	procBeginPaint         = user32.NewProc("BeginPaint")
	procEndPaint           = user32.NewProc("EndPaint")
	procFillRect           = user32.NewProc("FillRect")
	procDrawTextW          = user32.NewProc("DrawTextW")
	procSetWindowRgn       = user32.NewProc("SetWindowRgn")
	procMonitorFromPoint   = user32.NewProc("MonitorFromPoint")
	procGetMonitorInfoW    = user32.NewProc("GetMonitorInfoW")
	procSetLayeredWinAttrs = user32.NewProc("SetLayeredWindowAttributes")

	procCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject           = gdi32.NewProc("SelectObject")
	procDeleteObject           = gdi32.NewProc("DeleteObject")
	procDeleteDC               = gdi32.NewProc("DeleteDC")
	procBitBlt                 = gdi32.NewProc("BitBlt")
	procCreateSolidBrush       = gdi32.NewProc("CreateSolidBrush")
	procCreateFontW            = gdi32.NewProc("CreateFontW")
	procSetBkMode              = gdi32.NewProc("SetBkMode")
	procSetTextColor           = gdi32.NewProc("SetTextColor")
	procCreateRoundRectRgn     = gdi32.NewProc("CreateRoundRectRgn")
	procFillRgn                = gdi32.NewProc("FillRgn")

	procGetDpiForMonitor = shcore.NewProc("GetDpiForMonitor")
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
)

const (
	osdWmApp        = 0x8000 + 1 // WM_APP + 1: refresh and show
	osdWmPaint      = 0x000F
	osdWmEraseBkgnd = 0x0014
	osdWmTimer      = 0x0113

	osdWsPopup          = 0x80000000
	osdWsExTopmost      = 0x00000008
	osdWsExTransparent  = 0x00000020
	osdWsExToolWindow   = 0x00000080
	osdWsExLayered      = 0x00080000
	osdWsExNoActivate   = 0x08000000
	osdSwpNoActivate    = 0x0010
	osdSwpShowWindow    = 0x0040
	osdSwHide           = 0
	osdLwaAlpha         = 0x2
	osdSrcCopy          = 0x00CC0020
	osdTransparentBk    = 1
	osdDtLeft           = 0x0
	osdDtRight          = 0x2
	osdDtVCenter        = 0x4
	osdDtSingleLine     = 0x20
	osdDtEndEllipsis    = 0x8000
	osdDtNoPrefix       = 0x800
	osdHideTimerID      = 1
	osdBaseWidth        = 320
	osdBaseHeight       = 64
	osdBaseBottomMargin = 24
)

type osdRect struct{ Left, Top, Right, Bottom int32 }

type osdMonitorInfo struct {
	CbSize    uint32
	RcMonitor osdRect
	RcWork    osdRect
	DwFlags   uint32
}

type osdPaintStruct struct {
	Hdc         uintptr
	FErase      int32
	RcPaint     osdRect
	FRestore    int32
	FIncUpdate  int32
	RgbReserved [32]byte
}

type osdMsg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
	_       uint32
}

type osdWndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type volumeOSD struct {
	logger *zap.SugaredLogger

	lock     sync.Mutex
	label    string
	value    float32
	duration uint32

	hwnd  uintptr
	scale float64
	width  int32
	height int32
}

// only one OSD window per process - the window procedure needs to find it
var activeOSD *volumeOSD

func rgb(r, g, b uint8) uintptr { return uintptr(r) | uintptr(g)<<8 | uintptr(b)<<16 }

func newVolumeOSD(logger *zap.SugaredLogger) *volumeOSD {
	return &volumeOSD{logger: logger.Named("osd"), scale: 1}
}

func (o *volumeOSD) start() {
	activeOSD = o
	go o.loop()
}

// Show displays (or refreshes) the indicator with the given label and 0..1 volume value
func (o *volumeOSD) Show(label string, value float32, durationMs int) {
	o.lock.Lock()
	o.label = label
	o.value = value
	o.duration = uint32(durationMs)
	hwnd := o.hwnd
	o.lock.Unlock()

	if hwnd != 0 {
		procPostMessageW.Call(hwnd, osdWmApp, 0, 0)
	}
}

func (o *volumeOSD) loop() {
	// Win32 windows belong to the thread that created them
	runtime.LockOSThread()

	className, _ := syscall.UTF16PtrFromString("DeejVolumeOSD")
	hInstance, _, _ := procGetModuleHandleW.Call(0)

	wc := osdWndClassEx{
		LpfnWndProc:   syscall.NewCallback(osdWndProc),
		HInstance:     hInstance,
		LpszClassName: className,
	}
	wc.CbSize = uint32(unsafe.Sizeof(wc))

	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		o.logger.Warnw("Failed to register OSD window class", "error", err)
		return
	}

	hwnd, _, err := procCreateWindowExW.Call(
		osdWsExTopmost|osdWsExTransparent|osdWsExToolWindow|osdWsExLayered|osdWsExNoActivate,
		uintptr(unsafe.Pointer(className)), 0, osdWsPopup,
		0, 0, osdBaseWidth, osdBaseHeight, 0, 0, hInstance, 0)

	if hwnd == 0 {
		o.logger.Warnw("Failed to create OSD window", "error", err)
		return
	}

	procSetLayeredWinAttrs.Call(hwnd, 0, 240, osdLwaAlpha)

	o.lock.Lock()
	o.hwnd = hwnd
	o.lock.Unlock()

	o.logger.Debug("Volume OSD ready")

	var msg osdMsg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func osdWndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	o := activeOSD

	switch msg {
	case osdWmApp:
		o.reposition(hwnd)
		procInvalidateRect.Call(hwnd, 0, 0)
		o.lock.Lock()
		duration := o.duration
		o.lock.Unlock()
		procSetTimer.Call(hwnd, osdHideTimerID, uintptr(duration), 0)
		return 0

	case osdWmTimer:
		procKillTimer.Call(hwnd, osdHideTimerID)
		procShowWindow.Call(hwnd, osdSwHide)
		return 0

	case osdWmEraseBkgnd:
		return 1

	case osdWmPaint:
		o.paint(hwnd)
		return 0
	}

	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
	return r
}

// places the window bottom-center on the primary monitor's work area (above the taskbar)
func (o *volumeOSD) reposition(hwnd uintptr) {
	const monitorDefaultToPrimary = 1
	hMonitor, _, _ := procMonitorFromPoint.Call(0, monitorDefaultToPrimary)

	var dpiX, dpiY uint32 = 96, 96
	if procGetDpiForMonitor.Find() == nil {
		procGetDpiForMonitor.Call(hMonitor, 0, uintptr(unsafe.Pointer(&dpiX)), uintptr(unsafe.Pointer(&dpiY)))
	}

	mi := osdMonitorInfo{}
	mi.CbSize = uint32(unsafe.Sizeof(mi))
	procGetMonitorInfoW.Call(hMonitor, uintptr(unsafe.Pointer(&mi)))

	scale := float64(dpiX) / 96.0
	w := int32(osdBaseWidth * scale)
	h := int32(osdBaseHeight * scale)
	x := mi.RcWork.Left + (mi.RcWork.Right-mi.RcWork.Left-w)/2
	y := mi.RcWork.Bottom - h - int32(osdBaseBottomMargin*scale)

	if w != o.width || h != o.height {
		radius := uintptr(16 * scale)
		rgn, _, _ := procCreateRoundRectRgn.Call(0, 0, uintptr(w+1), uintptr(h+1), radius, radius)
		procSetWindowRgn.Call(hwnd, rgn, 1) // system owns the region afterwards
	}

	o.scale, o.width, o.height = scale, w, h

	const hwndTopmost = ^uintptr(0)
	procSetWindowPos.Call(hwnd, hwndTopmost, uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		osdSwpNoActivate|osdSwpShowWindow)
}

func osdFont(face string, height float64, weight int) uintptr {
	f, _ := syscall.UTF16PtrFromString(face)
	const defaultCharset, clearTypeQuality = 1, 5
	h, _, _ := procCreateFontW.Call(uintptr(-int32(height)), 0, 0, 0, uintptr(weight),
		0, 0, 0, defaultCharset, 0, 0, clearTypeQuality, 0, uintptr(unsafe.Pointer(f)))
	return h
}

func osdDrawText(hdc uintptr, text string, r osdRect, flags uintptr) {
	t, _ := syscall.UTF16FromString(text)
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(&t[0])), ^uintptr(0), uintptr(unsafe.Pointer(&r)), flags)
}

func osdFill(hdc uintptr, r osdRect, color uintptr, radius int32) {
	brush, _, _ := procCreateSolidBrush.Call(color)
	defer procDeleteObject.Call(brush)

	if radius <= 0 {
		procFillRect.Call(hdc, uintptr(unsafe.Pointer(&r)), brush)
		return
	}

	rgn, _, _ := procCreateRoundRectRgn.Call(uintptr(r.Left), uintptr(r.Top), uintptr(r.Right+1), uintptr(r.Bottom+1),
		uintptr(radius), uintptr(radius))
	procFillRgn.Call(hdc, rgn, brush)
	procDeleteObject.Call(rgn)
}

func (o *volumeOSD) paint(hwnd uintptr) {
	var ps osdPaintStruct
	hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	defer procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))

	o.lock.Lock()
	label, value := o.label, o.value
	o.lock.Unlock()

	s := o.scale
	w, h := o.width, o.height
	px := func(v float64) int32 { return int32(v * s) }

	// double buffer to avoid flicker while sliding
	mem, _, _ := procCreateCompatibleDC.Call(hdc)
	bmp, _, _ := procCreateCompatibleBitmap.Call(hdc, uintptr(w), uintptr(h))
	oldBmp, _, _ := procSelectObject.Call(mem, bmp)

	osdFill(mem, osdRect{0, 0, w, h}, rgb(32, 32, 32), 0)
	procSetBkMode.Call(mem, osdTransparentBk)
	procSetTextColor.Call(mem, rgb(255, 255, 255))

	// speaker glyph from Segoe MDL2 Assets, picked by volume level
	glyph := ""
	switch {
	case value <= 0:
		glyph = ""
	case value < 0.34:
		glyph = ""
	case value < 0.67:
		glyph = ""
	}

	iconFont := osdFont("Segoe MDL2 Assets", 20*s, 400)
	oldFont, _, _ := procSelectObject.Call(mem, iconFont)
	osdDrawText(mem, glyph, osdRect{px(18), 0, px(50), h}, osdDtLeft|osdDtVCenter|osdDtSingleLine)

	// slider name
	labelFont := osdFont("Segoe UI", 13*s, 400)
	procSelectObject.Call(mem, labelFont)
	procSetTextColor.Call(mem, rgb(200, 200, 200))
	osdDrawText(mem, label, osdRect{px(56), px(10), w - px(64), px(32)},
		osdDtLeft|osdDtVCenter|osdDtSingleLine|osdDtEndEllipsis|osdDtNoPrefix)

	// percent value
	percent := int(value*100 + 0.5)
	valueFont := osdFont("Segoe UI Semibold", 17*s, 600)
	procSelectObject.Call(mem, valueFont)
	procSetTextColor.Call(mem, rgb(255, 255, 255))
	osdDrawText(mem, strconv.Itoa(percent), osdRect{w - px(62), 0, w - px(18), h},
		osdDtRight|osdDtVCenter|osdDtSingleLine)

	// bar
	barLeft, barRight := px(56), w-px(72)
	barTop, barBottom := px(39), px(43)
	osdFill(mem, osdRect{barLeft, barTop, barRight, barBottom}, rgb(90, 90, 90), px(4))
	fillRight := barLeft + int32(float64(barRight-barLeft)*float64(value))
	if fillRight > barLeft {
		osdFill(mem, osdRect{barLeft, barTop, fillRight, barBottom}, rgb(76, 194, 255), px(4))
	}

	procBitBlt.Call(hdc, 0, 0, uintptr(w), uintptr(h), mem, 0, 0, osdSrcCopy)

	procSelectObject.Call(mem, oldFont)
	procDeleteObject.Call(iconFont)
	procDeleteObject.Call(labelFont)
	procDeleteObject.Call(valueFont)
	procSelectObject.Call(mem, oldBmp)
	procDeleteObject.Call(bmp)
	procDeleteDC.Call(mem)
}

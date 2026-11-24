package deej

import (
	"fmt"
	"syscall"
	"unsafe"
)

// theme colors and app icons for the volume OSD

var (
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	procRegGetValueW               = advapi32.NewProc("RegGetValueW")
	procOpenProcess                = kernel32.NewProc("OpenProcess")
	procCloseHandle                = kernel32.NewProc("CloseHandle")
	procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
	procPrivateExtractIconsW       = user32.NewProc("PrivateExtractIconsW")
	procDrawIconEx                 = user32.NewProc("DrawIconEx")
	procFrameRgn                   = gdi32.NewProc("FrameRgn")
)

const (
	osdHKeyCurrentUser = 0x80000001
	osdRrfRtRegBinary  = 0x08
	osdRrfRtRegDword   = 0x10

	osdProcessQueryLimitedInformation = 0x1000
	osdDiNormal                       = 0x3

	personalizeRegKey = `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`
	accentRegKey      = `Software\Microsoft\Windows\CurrentVersion\Explorer\Accent`
)

type osdTheme struct {
	background uintptr
	border     uintptr
	label      uintptr
	value      uintptr
	track      uintptr
	fill       uintptr
}

// reads the same settings the Windows flyouts use: system light/dark mode and the accent palette
func currentOSDTheme() osdTheme {
	light := false
	if v, ok := regDWORD(personalizeRegKey, "SystemUsesLightTheme"); ok {
		light = v == 1
	}

	return osdThemeFor(light)
}

func osdThemeFor(light bool) osdTheme {
	// AccentPalette holds 8 RGBA colors: light3, light2, light1, base, dark1, dark2, dark3, extra.
	// like Windows 11 controls, use light2 on dark backgrounds and dark1 on light ones
	palette := regBinary(accentRegKey, "AccentPalette")
	accent := func(idx int, fallback uintptr) uintptr {
		if len(palette) < 32 {
			return fallback
		}
		return rgb(palette[idx*4], palette[idx*4+1], palette[idx*4+2])
	}

	if light {
		return osdTheme{
			background: rgb(249, 249, 249),
			border:     rgb(222, 222, 222),
			label:      rgb(92, 92, 92),
			value:      rgb(0, 0, 0),
			track:      rgb(138, 138, 138),
			fill:       accent(4, rgb(0, 95, 184)),
		}
	}

	return osdTheme{
		background: rgb(44, 44, 44),
		border:     rgb(66, 66, 66),
		label:      rgb(200, 200, 200),
		value:      rgb(255, 255, 255),
		track:      rgb(154, 154, 154),
		fill:       accent(1, rgb(76, 194, 255)),
	}
}

func regDWORD(key, value string) (uint32, bool) {
	k, _ := syscall.UTF16PtrFromString(key)
	v, _ := syscall.UTF16PtrFromString(value)

	var data uint32
	size := uint32(unsafe.Sizeof(data))
	r, _, _ := procRegGetValueW.Call(osdHKeyCurrentUser, uintptr(unsafe.Pointer(k)), uintptr(unsafe.Pointer(v)),
		osdRrfRtRegDword, 0, uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&size)))

	return data, r == 0
}

func regBinary(key, value string) []byte {
	k, _ := syscall.UTF16PtrFromString(key)
	v, _ := syscall.UTF16PtrFromString(value)

	data := make([]byte, 64)
	size := uint32(len(data))
	r, _, _ := procRegGetValueW.Call(osdHKeyCurrentUser, uintptr(unsafe.Pointer(k)), uintptr(unsafe.Pointer(v)),
		osdRrfRtRegBinary, 0, uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(&size)))

	if r != 0 {
		return nil
	}

	return data[:size]
}

// icons are cached per executable path and size, forever - there are only a handful of mapped apps.
// only ever accessed from the OSD window thread
var osdIconCache = map[string]uintptr{}

// returns an icon handle for the executable of the given process, or 0 if there isn't one
func osdIconForProcess(pid uint32, size int32) uintptr {
	if pid == 0 {
		return 0
	}

	path := processImagePath(pid)
	if path == "" {
		return 0
	}

	cacheKey := fmt.Sprintf("%s|%d", path, size)
	if icon, ok := osdIconCache[cacheKey]; ok {
		return icon
	}

	p, _ := syscall.UTF16PtrFromString(path)
	var icon uintptr
	var iconID uint32
	count, _, _ := procPrivateExtractIconsW.Call(uintptr(unsafe.Pointer(p)), 0, uintptr(size), uintptr(size),
		uintptr(unsafe.Pointer(&icon)), uintptr(unsafe.Pointer(&iconID)), 1, 0)

	if count == 0 || count == ^uintptr(0) {
		icon = 0
	}

	osdIconCache[cacheKey] = icon
	return icon
}

func processImagePath(pid uint32) string {
	handle, _, _ := procOpenProcess.Call(osdProcessQueryLimitedInformation, 0, uintptr(pid))
	if handle == 0 {
		return ""
	}
	defer procCloseHandle.Call(handle)

	buf := make([]uint16, syscall.MAX_LONG_PATH)
	size := uint32(len(buf))
	if r, _, _ := procQueryFullProcessImageNameW.Call(handle, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r == 0 {
		return ""
	}

	return syscall.UTF16ToString(buf[:size])
}

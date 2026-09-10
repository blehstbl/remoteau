//go:build windows

// Package wasapi is a pure-Go (cgo-free) WASAPI backend for Windows using
// COM via golang.org/x/sys/windows. It provides loopback capture of render
// endpoints, microphone capture, and shared-mode render playback.
package wasapi

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ole32                = windows.NewLazySystemDLL("ole32.dll")
	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32.NewProc("CoUninitialize")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFree    = ole32.NewProc("CoTaskMemFree")
)

const (
	clsCtxAll               = 0x17 // CLSCTX_ALL
	coinitMultithreded      = 0x0
	coinitApartmentThreaded = 0x2
)

// GUIDs (Windows SDK)
var (
	clsidMMDeviceEnumerator = newGUID("{BCDE0395-E52F-467C-8E3D-C4579291692E}")
	iidIMMDeviceEnumerator  = newGUID("{A95664D2-9614-4F35-A746-DE8DB63617E6}")
	iidIMMDeviceCollection  = newGUID("{0BD7A1BE-7A1A-44DB-8397-CC5392387B5E}")
	iidIAudioClient         = newGUID("{1CB9AD4C-DBFA-4C32-B178-C2F568A703B2}")
	iidIAudioCaptureClient  = newGUID("{C8ADBD64-E71E-48A0-A4DE-185C395CD317}")
	iidIAudioRenderClient   = newGUID("{F294ACFC-3146-4483-A7BF-ADDCA7C260E2}")
	iidIPropertyStore       = newGUID("{886D8EEB-8CF2-4446-8D02-CDBA1DBDCF99}")

	subFormatIEEEFloat = newGUID("{00000003-0000-0010-8000-00AA00389B71}")
	subFormatPCM       = newGUID("{00000001-0000-0010-8000-00AA00389B71}")
)

// Data flow / role constants.
const (
	dataFlowRender  = 0
	dataFlowCapture = 1
	roleConsole     = 0
	roleMultimedia  = 1
)

// WASAPI constants.
const (
	shareModeShared = 0

	streamFlagsEventCallback = 0x00040000
	streamFlagsLoopback      = 0x00020000
	streamFlagsNoPersist     = 0x00080000

	bufferFlagsSilent        = 0x2
	bufferFlagsDiscontinuity = 0x1

	stgmRead = 0

	stateActive = 0x1
)

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

func newGUID(s string) *guid {
	g := &guid{}
	str := []byte(s)
	// Parse "{XXXXXXXX-XXXX-XXXX-XXXX-XXXXXXXXXXXX}"
	if str[0] == '{' {
		str = str[1:]
	}
	parseHex := func(b []byte, n int) uint64 {
		var v uint64
		for i := 0; i < n; i++ {
			c := b[i]
			var d uint64
			switch {
			case c >= '0' && c <= '9':
				d = uint64(c - '0')
			case c >= 'a' && c <= 'f':
				d = uint64(c-'a') + 10
			case c >= 'A' && c <= 'F':
				d = uint64(c-'A') + 10
			default:
				d = 0
			}
			v = v<<4 | d
		}
		return v
	}
	g.Data1 = uint32(parseHex(str[0:8], 8))
	g.Data2 = uint16(parseHex(str[9:13], 4))
	g.Data3 = uint16(parseHex(str[14:18], 4))
	for i := 0; i < 2; i++ {
		g.Data4[i] = byte(parseHex(str[19+i*2:21+i*2], 2))
	}
	for i := 0; i < 6; i++ {
		g.Data4[2+i] = byte(parseHex(str[24+i*2:26+i*2], 2))
	}
	return g
}

func coInitialize() error {
	hr, _, _ := procCoInitializeEx.Call(0, coinitMultithreded)
	// S_OK, S_FALSE, RPC_E_CHANGED_MODE are all acceptable to proceed.
	if hr == uintptr(0x80010106) { // RPC_E_CHANGED_MODE
		return nil
	}
	if hr != 0 && hr != 1 {
		return fmt.Errorf("CoInitializeEx failed: 0x%08x", hr)
	}
	return nil
}

func coUninitialize() {
	procCoUninitialize.Call()
}

func coTaskMemFree(p unsafe.Pointer) {
	if p != nil {
		procCoTaskMemFree.Call(uintptr(p))
	}
}

// vtable returns the COM object's method table as a uintptr slice.
func vtable(obj unsafe.Pointer) []uintptr {
	return (*[64]uintptr)(*(**[64]uintptr)(obj))[:]
}

func callCom(fn uintptr, obj unsafe.Pointer, args ...uintptr) uintptr {
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, uintptr(unsafe.Pointer(obj)))
	all = append(all, args...)
	r1, _, _ := syscall.SyscallN(fn, all...)
	return r1
}

func coCreateInstance(clsid, iid *guid) (unsafe.Pointer, error) {
	var out unsafe.Pointer
	hr, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(clsid)),
		0,
		clsCtxAll,
		uintptr(unsafe.Pointer(iid)),
		uintptr(unsafe.Pointer(&out)),
	)
	if hr != 0 {
		return nil, fmt.Errorf("CoCreateInstance failed: 0x%08x", hr)
	}
	if out == nil {
		return nil, fmt.Errorf("CoCreateInstance returned nil")
	}
	return out, nil
}

func hrErr(what string, hr uintptr) error {
	return fmt.Errorf("%s failed: 0x%08x", what, hr)
}

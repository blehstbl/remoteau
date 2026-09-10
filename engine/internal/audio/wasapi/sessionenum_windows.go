//go:build windows

package wasapi

import (
	"fmt"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"remote-au/internal/audio"
)

// Audio session enumeration for per-app capture pickers (Phase 9).
//
// Chain: default render endpoint → IAudioSessionManager2 →
// IAudioSessionEnumerator → IAudioSessionControl (QI → IAudioSessionControl2
// for GetProcessID). Display names fall back to the process image name via
// kernel32 OpenProcess + QueryFullProcessImageNameW.
//
// LIVE VALIDATION PENDING: this requires a live default render endpoint; the
// build machine has zero audio endpoints, so only compile-cleanliness has
// been verified so far.

// IIDs (audiopolicy.h, Windows SDK).
var (
	iidIAudioSessionManager2   = newGUID("{77AAFC35-F1CB-4D17-9CD9-3EB8E0BD6533}")
	iidIAudioSessionEnumerator = newGUID("{E2F5BB66-D3CA-4E9C-A5C7-A0B0E4B7C2E8}")
	iidIAudioSessionControl2   = newGUID("{BFB7FF88-7239-4FC9-8FA2-07C950BE9C6D}")

	procQueryFullProcessImageNameW = windows.NewLazySystemDLL("kernel32.dll").
					NewProc("QueryFullProcessImageNameW")
)

// AudioSessionState values (audiopolicy.h).
const (
	sessionStateInactive = 0
	sessionStateActive   = 1
	sessionStateExpired  = 2
)

// ProcessInfo describes one process owning an audio session.
type ProcessInfo struct {
	PID         uint32
	DisplayName string
	State       int
}

// ListAudioProcesses lists the processes that currently have ACTIVE audio
// sessions on the default render endpoint (state == sessionStateActive).
func ListAudioProcesses() ([]ProcessInfo, error) {
	if err := coInitialize(); err != nil {
		return nil, err
	}
	defer coUninitialize()

	enum, err := newMMDeviceEnumerator()
	if err != nil {
		return nil, err
	}
	defer enum.release()

	dev, err := enum.getDefaultAudioEndpoint(dataFlowRender, roleConsole)
	if err != nil {
		return nil, fmt.Errorf("default render endpoint: %w", err)
	}
	defer dev.release()

	mgr, err := dev.activate(iidIAudioSessionManager2)
	if err != nil {
		return nil, fmt.Errorf("activate IAudioSessionManager2: %w", err)
	}
	defer callCom(vtable(mgr)[2], mgr) // Release

	// IAudioSessionManager2 vtable: IUnknown(0..2), GetAudioSessionControl(3),
	// GetSimpleAudioVolume(4), GetSessionEnumerator(5).
	var sessEnum unsafe.Pointer
	if hr := callCom(vtable(mgr)[5], mgr, uintptr(unsafe.Pointer(&sessEnum))); hr != 0 {
		return nil, hrErr("IAudioSessionManager2::GetSessionEnumerator", hr)
	}
	defer callCom(vtable(sessEnum)[2], sessEnum) // Release

	// IAudioSessionEnumerator vtable: IUnknown(0..2), GetCount(3), GetSession(4).
	var count uint32
	if hr := callCom(vtable(sessEnum)[3], sessEnum, uintptr(unsafe.Pointer(&count))); hr != 0 {
		return nil, hrErr("IAudioSessionEnumerator::GetCount", hr)
	}

	out := make([]ProcessInfo, 0, count)
	for i := uint32(0); i < count; i++ {
		var sess unsafe.Pointer
		if hr := callCom(vtable(sessEnum)[4], sessEnum, uintptr(i), uintptr(unsafe.Pointer(&sess))); hr != 0 {
			continue
		}
		info, ok := sessionInfo(sess)
		callCom(vtable(sess)[2], sess) // Release
		if ok && info.State == sessionStateActive {
			out = append(out, info)
		}
	}
	return out, nil
}

// ListAudioProcesses implements audio.ProcessLister so UIs can discover the
// capability by type-asserting a Backend. It delegates to the package-level
// enumeration, which performs its own COM initialization on the calling
// thread, and converts to the platform-neutral type. The OS thread is pinned
// so the CoInitializeEx and the COM calls that follow run in the same COM
// apartment.
func (b *Backend) ListAudioProcesses() ([]audio.AudioProcessInfo, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	infos, err := ListAudioProcesses()
	if err != nil {
		return nil, err
	}
	out := make([]audio.AudioProcessInfo, 0, len(infos))
	for _, in := range infos {
		out = append(out, audio.AudioProcessInfo{PID: in.PID, Name: in.DisplayName, State: in.State})
	}
	return out, nil
}

// sessionInfo extracts (pid, display name, state) from one
// IAudioSessionControl, QI'd to IAudioSessionControl2 for GetProcessID.
func sessionInfo(sess unsafe.Pointer) (ProcessInfo, bool) {
	// IAudioSessionControl vtable: IUnknown(0..2), GetState(3),
	// GetDisplayName(4), SetDisplayName(5), GetIconPath(6), SetIconPath(7),
	// GetGroupingParam(8), SetGroupingParam(9),
	// Register/UnregisterAudioSessionNotification(10/11).
	var state uint32
	if hr := callCom(vtable(sess)[3], sess, uintptr(unsafe.Pointer(&state))); hr != 0 {
		return ProcessInfo{}, false
	}

	var c2 unsafe.Pointer
	if hr := callCom(vtable(sess)[0], sess, // QueryInterface
		uintptr(unsafe.Pointer(iidIAudioSessionControl2)),
		uintptr(unsafe.Pointer(&c2))); hr != 0 || c2 == nil {
		return ProcessInfo{}, false
	}
	defer callCom(vtable(c2)[2], c2) // Release

	// IAudioSessionControl2 adds GetSessionIdentifier(12),
	// GetSessionInstanceIdentifier(13), GetProcessID(14),
	// IsSystemSoundsSession(15), SetDuckingPriority(16).
	var pid uint32
	if hr := callCom(vtable(c2)[14], c2, uintptr(unsafe.Pointer(&pid))); hr != 0 {
		return ProcessInfo{}, false
	}

	// GetDisplayName returns a CoTaskMemAlloc'd LPWSTR; apps rarely set it,
	// so fall back to the process image name.
	name := ""
	var pwsz unsafe.Pointer
	if hr := callCom(vtable(sess)[4], sess, uintptr(unsafe.Pointer(&pwsz))); hr == 0 && pwsz != nil {
		name = utf16PtrToString(pwsz)
		coTaskMemFree(pwsz)
	}
	if name == "" {
		name = processImageName(pid)
	}
	return ProcessInfo{PID: pid, DisplayName: name, State: int(state)}, true
}

// processImageName returns the executable file name (not the full path) for
// a PID, or "" when the process cannot be opened (rights/lifetime races are
// normal here).
func processImageName(pid uint32) string {
	if pid == 0 {
		return ""
	}
	h, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		return ""
	}
	defer windows.CloseHandle(windows.Handle(h))

	var buf [1024]uint16
	size := uint32(len(buf))
	ret, _, _ := procQueryFullProcessImageNameW.Call(
		h, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if ret == 0 || size == 0 || int(size) > len(buf) {
		return ""
	}
	full := windows.UTF16ToString(buf[:size])
	if i := strings.LastIndexByte(full, '\\'); i >= 0 && i+1 < len(full) {
		return full[i+1:]
	}
	return full
}

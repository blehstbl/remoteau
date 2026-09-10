//go:build windows

package wasapi

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"remote-au/internal/audio"
	"remote-au/internal/logging"
)

// Process loopback (per-application capture, Phase 9).
//
// Windows exposes per-process audio capture through
// combase!ActivateAudioInterfaceAsync with an AUDIOCLIENT_ACTIVATION_PARAMS
// PROPVARIANT (AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK). This requires
// Windows 10 2004 (build 19041) or newer; a missing entry point or a failing
// HRESULT is surfaced as a runtime error — we never fake audio.
//
// LIVE VALIDATION PENDING: the build machine currently has ZERO active audio
// endpoints, so this COM path could not be exercised yet. It is compile-clean
// and reviewed against the SDK contract, but behavior against live endpoints
// must be verified on a real system before shipping.
//
// Notes:
//   - AUDCLNT_STREAMFLAGS_LOOPBACK is deliberately NOT set: the process
//     loopback activation type replaces endpoint loopback entirely. Shared
//     mode + the same packet-poll loop as OpenCapture is used (event
//     callbacks are optional for capture; polling is fine).
//   - TargetProcessId is a single PID per activation. PROCESS_TARGET mode
//     with multiple PIDs opens one activation per PID and mixes the streams
//     (sum with clipping). EXCLUDE_PROCESS_TARGET takes exactly one PID;
//     requesting more than one is rejected (API limitation, documented).

var (
	combase                         = windows.NewLazySystemDLL("combase.dll")
	procActivateAudioInterfaceAsync = combase.NewProc("ActivateAudioInterfaceAsync")
	kernel32                        = windows.NewLazySystemDLL("kernel32.dll")
	procOpenProcess                 = kernel32.NewProc("OpenProcess")
	procQueryFullProcessImageNameW  = kernel32.NewProc("QueryFullProcessImageNameW")
)

// GUIDs (Windows SDK). iidIAudioClient lives in com.go.
var (
	iidIUnknown     = newGUID("{00000000-0000-0000-C000-000000000046}")
	iidIAgileObject = newGUID("{94EA2B94-E9CC-49E0-C0FF-EE64CA8F5B90}") // objbase.h
)

// Process-loopback constants (audioclient.h / combase activation API).
const (
	vtUI8 = 21 // VT_UI8 PROPVARIANT tag

	// AUDIOCLIENT_ACTIVATION_TYPE: 0 is INVALID, 1 is PROCESS_LOOPBACK.
	audioClientActivationTypeProcessLoopback = 1

	// PROCESS_LOOPBACK_MODE.
	processLoopbackModeProcessTarget        = 0 // capture only the target process
	processLoopbackModeExcludeProcessTarget = 1 // capture everything except the target

	eNoInterface = uintptr(0x80004002) // E_NOINTERFACE
	ePointer     = uintptr(0x80004003) // E_POINTER
	eFail        = uintptr(0x80004005) // E_FAIL

	// processLoopbackActivateTimeout bounds the async activation wait; the
	// completion normally arrives within milliseconds.
	processLoopbackActivateTimeout = 5 * time.Second

	// processQueryLimitedInformation for OpenProcess (name lookup only).
	processQueryLimitedInformation = 0x1000
)

// validateProcessLoopbackTargets checks and normalizes the PID list before
// any COM work: process-target needs at least one PID, exclude-target is
// limited to a single PID (Windows takes one PID per activation), and PID 0
// is invalid. Returns a deduped copy.
func validateProcessLoopbackTargets(pids []uint32, exclude bool) ([]uint32, error) {
	if len(pids) == 0 {
		return nil, fmt.Errorf("process loopback: no target process ids (exclude=%v)", exclude)
	}
	seen := make(map[uint32]bool, len(pids))
	out := make([]uint32, 0, len(pids))
	for _, p := range pids {
		if p == 0 {
			return nil, fmt.Errorf("process loopback: pid 0 is invalid")
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	if exclude && len(out) > 1 {
		return nil, fmt.Errorf("process loopback: exclude mode supports a single pid, got %d "+
			"(the API takes one target pid per activation)", len(out))
	}
	return out, nil
}

// activateCompletionHandler implements IActivateAudioInterfaceCompletionHandler
// (and the IAgileObject marker interface, required by ActivateAudioInterfaceAsync
// to avoid activation deadlocks) as a Go-managed COM object. The object
// pointer passed to COM is the struct address; its first field is the vtable
// pointer, matching vtable()'s pointer-to-pointer expectations.
//
// Layout contract: COM sees [vt, ...]; the callbacks recover the struct by
// casting `this` back. The vtable entries are syscall.NewCallback trampolines
// created once at package level (NewCallback must not be called per object).
type activateCompletionHandler struct {
	vt *[4]uintptr // QueryInterface, AddRef, Release, ActivateCompleted

	refs   int32 // COM refcount bookkeeping; the object itself is never freed (Go GC)
	once   sync.Once
	done   chan struct{} // closed exactly once by ActivateCompleted
	hr     uintptr       // GetResult HRESULT (valid after done)
	result unsafe.Pointer
}

var (
	activateCallbackOnce sync.Once
	cbActivateQI         uintptr
	cbActivateAddRef     uintptr
	cbActivateRelease    uintptr
	cbActivateCompleted  uintptr
)

// cbPtr converts a COM callback's uintptr argument (this/riid/out) back into
// a pointer. go vet's unsafeptr check flags this conversion by design; it is
// the one intentional, documented exception in this package: the referents
// are live heap objects (Go's GC does not move them) produced from pointers
// on the same call stack.
func cbPtr(u uintptr) unsafe.Pointer { return unsafe.Pointer(u) }

func initActivateCallbacks() {
	activateCallbackOnce.Do(func() {
		cbActivateQI = windows.NewCallback(activateQueryInterface)
		cbActivateAddRef = windows.NewCallback(activateAddRef)
		cbActivateRelease = windows.NewCallback(activateRelease)
		cbActivateCompleted = windows.NewCallback(activateCompleted)
	})
}

func newActivateCompletionHandler() *activateCompletionHandler {
	initActivateCallbacks()
	return &activateCompletionHandler{
		vt:   &[4]uintptr{cbActivateQI, cbActivateAddRef, cbActivateRelease, cbActivateCompleted},
		done: make(chan struct{}),
	}
}

// activateQueryInterface answers IUnknown and IAgileObject with the same
// object pointer; everything else is E_NOINTERFACE.
func activateQueryInterface(this, riid, out uintptr) uintptr {
	if riid == 0 || out == 0 {
		return ePointer
	}
	h := (*activateCompletionHandler)(cbPtr(this))
	g := (*guid)(cbPtr(riid))
	if *g == *iidIUnknown || *g == *iidIAgileObject {
		*(**activateCompletionHandler)(cbPtr(out)) = h
		atomic.AddInt32(&h.refs, 1)
		return 0 // S_OK
	}
	*(*unsafe.Pointer)(cbPtr(out)) = nil
	return eNoInterface
}

func activateAddRef(this uintptr) uintptr {
	h := (*activateCompletionHandler)(cbPtr(this))
	return uintptr(atomic.AddInt32(&h.refs, 1))
}

func activateRelease(this uintptr) uintptr {
	h := (*activateCompletionHandler)(cbPtr(this))
	// The Go object outlives COM (never freed); just track the count.
	return uintptr(atomic.AddInt32(&h.refs, -1))
}

// activateCompleted runs on a COM/RPC thread when activation finishes. It
// pulls the IAudioClient out via IActivateAudioInterfaceOperation::GetResult
// (vtable slot 3) and wakes the waiting goroutine. Must stay short and must
// not take locks the waiter holds (it doesn't take any).
func activateCompleted(this, op uintptr) uintptr {
	h := (*activateCompletionHandler)(cbPtr(this))
	hr := eFail
	var out unsafe.Pointer
	if op != 0 {
		hr = callCom(vtable(cbPtr(op))[3], cbPtr(op),
			uintptr(unsafe.Pointer(iidIAudioClient)), uintptr(unsafe.Pointer(&out)))
	}
	h.once.Do(func() {
		h.hr = hr
		h.result = out
		close(h.done)
	})
	return 0 // S_OK
}

// activateProcessLoopbackClient activates ONE process-loopback IAudioClient
// for a single PID. Returns the IAudioClient pointer; the caller owns the
// reference (and releases it via audioClient.release()).
func activateProcessLoopbackClient(pid uint32, mode uint32) (unsafe.Pointer, error) {
	if err := procActivateAudioInterfaceAsync.Find(); err != nil {
		return nil, fmt.Errorf("process loopback requires Windows 10 2004+ "+
			"(combase!ActivateAudioInterfaceAsync missing): %w", err)
	}

	// AUDIOCLIENT_ACTIVATION_PARAMS (audioclient.h): ActivationType (u32
	// enum) at offset 0, then the ProcessLoopbackParams union
	// {TargetProcessId u32; ProcessLoopbackMode u32} at offset 4 (union
	// alignment is 4 — both members are DWORDs); 12 bytes, padded to 16.
	var params [16]byte
	binary.LittleEndian.PutUint32(params[0:], audioClientActivationTypeProcessLoopback)
	binary.LittleEndian.PutUint32(params[4:], pid)
	binary.LittleEndian.PutUint32(params[8:], mode)

	// PROPVARIANT carrying the params pointer (VT_UI8): vt u16 at offset 0,
	// 6 bytes padding, the 8-byte union value at offset 8 (x64) — the same
	// layout mmdevice.go reads for friendlyName, constructed here.
	var pv [24]byte
	binary.LittleEndian.PutUint16(pv[0:], vtUI8)
	binary.LittleEndian.PutUint64(pv[8:], uint64(uintptr(unsafe.Pointer(&params[0]))))

	// deviceName: L"" (process loopback needs no endpoint).
	var emptyDeviceName [1]uint16

	h := newActivateCompletionHandler()
	var op unsafe.Pointer
	hr, _, _ := procActivateAudioInterfaceAsync.Call(
		uintptr(unsafe.Pointer(&emptyDeviceName[0])),
		uintptr(unsafe.Pointer(iidIAudioClient)),
		uintptr(unsafe.Pointer(&pv[0])),
		uintptr(unsafe.Pointer(h)),
		uintptr(unsafe.Pointer(&op)),
	)
	runtime.KeepAlive(&params)
	runtime.KeepAlive(&pv)
	runtime.KeepAlive(&emptyDeviceName)
	if hr != 0 {
		return nil, hrErr("ActivateAudioInterfaceAsync", hr)
	}
	if op == nil {
		return nil, fmt.Errorf("ActivateAudioInterfaceAsync returned no completion operation")
	}
	defer callCom(vtable(op)[2], op) // IActivateAudioInterfaceOperation::Release

	select {
	case <-h.done:
	case <-time.After(processLoopbackActivateTimeout):
		return nil, fmt.Errorf("process loopback activation timed out (pid %d)", pid)
	}
	if h.hr != 0 {
		return nil, hrErr(fmt.Sprintf("process loopback activation for pid %d (GetResult)", pid), h.hr)
	}
	if h.result == nil {
		return nil, fmt.Errorf("process loopback activation returned nil IAudioClient (pid %d)", pid)
	}
	return h.result, nil
}

// OpenProcessLoopback captures the audio of specific Windows processes (or
// system audio excluding them) via WASAPI process loopback. exclude=false
// captures ONLY targetPIDs (one activation per PID, mixed); exclude=true
// captures system audio minus targetPIDs (single PID only).
//
// LIVE VALIDATION PENDING: see the package comment above.
func OpenProcessLoopback(targetPIDs []uint32, exclude bool, format audio.Format, logger logging.Logger) (audio.Capture, error) {
	if err := format.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = logging.Nop()
	}
	pids, err := validateProcessLoopbackTargets(targetPIDs, exclude)
	if err != nil {
		return nil, err
	}

	if err := coInitialize(); err != nil {
		return nil, err
	}
	// COM is per-thread; capture goroutines initialize their own apartments.
	coUninitialize()

	mode := uint32(processLoopbackModeProcessTarget)
	if exclude {
		mode = processLoopbackModeExcludeProcessTarget
	}

	if len(pids) == 1 {
		obj, err := activateProcessLoopbackClient(pids[0], mode)
		if err != nil {
			return nil, err
		}
		return newPacketCapture(obj, streamFlagsNoPersist, audio.CaptureOptions{
			Format: format,
			Logger: logger,
		}, "process-loopback", fmt.Sprintf("pid %d", pids[0]))
	}

	// Multi-PID process-target: one activation per PID; the per-PID streams
	// (each converted to the engine S16 format) are mixed with clipping.
	streams := make([]*Capture, 0, len(pids))
	defer func() {
		for _, s := range streams {
			_ = s.Close()
		}
	}()
	for _, pid := range pids {
		obj, err := activateProcessLoopbackClient(pid, mode)
		if err != nil {
			return nil, fmt.Errorf("pid %d: %w", pid, err)
		}
		s, err := newPacketCapture(obj, streamFlagsNoPersist, audio.CaptureOptions{
			Format: format,
			Logger: logger,
		}, "process-loopback", fmt.Sprintf("pid %d", pid))
		if err != nil {
			return nil, fmt.Errorf("pid %d: %w", pid, err)
		}
		streams = append(streams, s)
	}

	m := &mixedCapture{
		format:  format,
		out:     audio.NewRing(format.Rate/4*format.BytesPerFrame(), format.BytesPerFrame()),
		streams: streams,
		pids:    pids,
		done:    make(chan struct{}),
		gone:    make(chan struct{}),
		logger:  logger,
	}
	go m.mixLoop()
	logger.Infof("process loopback mixing %d processes: %v", len(pids), pids)
	return m, nil
}

// mixedCapture fans multiple per-PID loopback captures into one S16 stream
// by frame-wise summation with clipping (mixer.go mixS16LE-style).
type mixedCapture struct {
	format    audio.Format
	out       *audio.Ring
	streams   []*Capture
	pids      []uint32
	done      chan struct{}
	gone      chan struct{}
	closeOnce sync.Once
	logger    logging.Logger
}

func (m *mixedCapture) Format() audio.Format { return m.format }

func (m *mixedCapture) Read(dst []byte) int { return m.out.Read(dst) }

func (m *mixedCapture) Start() error { return nil } // streams start themselves

func (m *mixedCapture) Close() error {
	m.closeOnce.Do(func() {
		close(m.done)
		<-m.gone
		for _, s := range m.streams {
			_ = s.Close()
		}
	})
	return nil
}

// mixLoop sums the per-PID streams frame-by-frame into the output ring. The
// min-available discipline locks all streams to the slowest one; a stream
// whose capture goroutine died (e.g. its process exited with an error) is
// dropped from the mix, and the mixer stops when every stream is gone.
func (m *mixedCapture) mixLoop() {
	defer close(m.gone)

	n := len(m.streams)
	frameBytes := m.format.BytesPerFrame()
	channels := m.format.Channels
	const chunkFrames = 480 // 10 ms at 48 kHz
	scratch := make([][]byte, n)
	for i := range scratch {
		scratch[i] = make([]byte, chunkFrames*frameBytes)
	}
	acc := make([]int32, chunkFrames*channels)
	out := make([]byte, chunkFrames*frameBytes)
	dead := make([]bool, n)
	alive := n

	for {
		select {
		case <-m.done:
			return
		default:
		}

		// Drop streams whose capture goroutine exited.
		for i := 0; i < n; i++ {
			if dead[i] {
				continue
			}
			select {
			case <-m.streams[i].gone:
				dead[i] = true
				alive--
				m.logger.Warnf("process loopback stream pid %d ended (%d of %d streams dead)",
					m.pids[i], n-alive, n)
			default:
			}
		}
		if alive == 0 {
			return
		}

		minFrames := chunkFrames
		for i := 0; i < n; i++ {
			if dead[i] {
				continue
			}
			got := m.streams[i].ring.Read(scratch[i])
			if frames := got / frameBytes; frames < minFrames {
				minFrames = frames
			}
		}
		if minFrames <= 0 {
			select {
			case <-m.done:
				return
			case <-time.After(capturePollInterval):
			}
			continue
		}

		// Sum with clipping, like mixer.go's mixS16LE.
		for f := 0; f < minFrames; f++ {
			for c := 0; c < channels; c++ {
				off := f*frameBytes + c*2
				var sum int32
				for i := 0; i < n; i++ {
					if dead[i] {
						continue
					}
					sum += int32(int16(binary.LittleEndian.Uint16(scratch[i][off:])))
				}
				acc[f*channels+c] = sum
			}
		}
		wb := 0
		for f := 0; f < minFrames; f++ {
			for c := 0; c < channels; c++ {
				binary.LittleEndian.PutUint16(out[wb:], clampI32ToS16(acc[f*channels+c]))
				wb += 2
			}
		}
		m.out.TryWrite(out[:minFrames*frameBytes])
	}
}

func clampI32ToS16(v int32) uint16 {
	switch {
	case v > 32767:
		return 32767
	case v < -32768:
		return 32768
	default:
		return uint16(int16(v))
	}
}

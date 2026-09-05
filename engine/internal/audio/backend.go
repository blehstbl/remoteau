package audio

import (
	"fmt"
	"runtime"

	"remote-au/internal/logging"
)

// Source selects what a capture stream captures.
type Source int

const (
	SourceMicrophone Source = iota
	SourceLoopback
)

// DeviceInfo describes one enumerated audio endpoint.
type DeviceInfo struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	ID    string `json:"id"`
	Note  string `json:"note,omitempty"`
}

// DeviceLists is the set of endpoints a backend can use.
type DeviceLists struct {
	Playback     []DeviceInfo `json:"playback"`
	Capture      []DeviceInfo `json:"capture"`
	LoopbackNote string       `json:"loopbackNote"`
}

// CaptureOptions describes a capture stream to open. DeviceSelector uses the
// same syntax as the CLI --device flag: index or case-insensitive name
// substring; for loopback on Windows it selects a playback endpoint.
type CaptureOptions struct {
	Format         Format
	Source         Source
	DeviceSelector string
	RingFrames     int
	Verbose        bool
	Logger         logging.Logger
}

// PlaybackOptions describes a playback stream. Pull is invoked from the
// real-time audio callback and must obey the real-time rules.
type PlaybackOptions struct {
	Format         Format
	DeviceSelector string
	Pull           PullFunc
	Verbose        bool
	Logger         logging.Logger
}

type PullFunc func(out []byte, frameCount uint32) int

// Capture is a started-or-startable capture stream producing S16LE PCM.
type Capture interface {
	Format() Format
	Read(dst []byte) int
	Start() error
	Close() error
}

// Playback is a pull-based playback stream.
type Playback interface {
	Format() Format
	Start() error
	Close() error
}

// Backend is a platform audio implementation. Backends must be safe to use
// from non-realtime goroutines; realtime rules apply only to Pull callbacks.
type Backend interface {
	Name() string
	SupportsLoopback() bool
	EnumerateDevices() (DeviceLists, error)
	OpenCapture(opts CaptureOptions) (Capture, error)
	OpenPlayback(opts PlaybackOptions) (Playback, error)
}

// ErrNoBackend is returned when the build has no audio backend for the
// platform or the requested one is unavailable.
var ErrNoBackend = fmt.Errorf("no audio backend available")

// backendRegistry maps backend names to constructors.
var backendRegistry = map[string]func() Backend{}

// RegisterBackend makes a backend available to OpenBackend/Backends. It is
// intended to be called from backend package init() hooks.
func RegisterBackend(name string, ctor func() Backend) {
	backendRegistry[name] = ctor
}

// Backends lists registered backend names in stable order.
func Backends() []string {
	names := make([]string, 0, len(backendRegistry))
	for name := range backendRegistry {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// DefaultBackendName returns the default backend for this platform/build.
func DefaultBackendName() string {
	return defaultBackendName(runtime.GOOS)
}

// OpenBackend opens a named backend, or the default when name is empty.
func OpenBackend(name string) (Backend, error) {
	if name == "" {
		name = DefaultBackendName()
	}
	ctor, ok := backendRegistry[name]
	if !ok {
		return nil, fmt.Errorf("%w: unknown backend %q (available: %v)", ErrNoBackend, name, Backends())
	}
	b := ctor()
	if b == nil {
		return nil, fmt.Errorf("%w: backend %q not built into this binary", ErrNoBackend, name)
	}
	return b, nil
}

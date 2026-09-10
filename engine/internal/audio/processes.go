package audio

// AudioProcessInfo describes one process that owns an active audio session on
// the system's default render endpoint. It is the platform-neutral view used
// by UI layers (e.g. the Windows tray per-application source picker).
type AudioProcessInfo struct {
	PID   uint32
	Name  string
	State int
}

// ProcessLister is an optional capability a Backend may implement to enumerate
// the processes currently producing audio. Callers type-assert a Backend to
// this interface before offering per-application capture:
//
//	if lister, ok := b.(audio.ProcessLister); ok { ... }
type ProcessLister interface {
	ListAudioProcesses() ([]AudioProcessInfo, error)
}

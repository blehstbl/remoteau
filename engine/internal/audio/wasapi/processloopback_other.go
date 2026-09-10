//go:build !windows

package wasapi

import (
	"errors"

	"remote-au/internal/audio"
	"remote-au/internal/logging"
)

// OpenProcessLoopback is only implemented on Windows (WASAPI process
// loopback via combase!ActivateAudioInterfaceAsync). This stub keeps the
// package importable from platform-neutral code and returns a clear error
// on every other platform.
func OpenProcessLoopback(targetPIDs []uint32, exclude bool, format audio.Format, logger logging.Logger) (audio.Capture, error) {
	return nil, errors.New("wasapi: process loopback capture is windows-only")
}

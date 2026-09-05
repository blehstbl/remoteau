//go:build malgo

package malgoaudio

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"github.com/gen2brain/malgo"

	"remote-au/internal/audio"
	"remote-au/internal/logging"
)

// EnumerateDevices lists playback and capture endpoints. On Windows, playback
// endpoints double as WASAPI loopback sources.
func (b *Backend) EnumerateDevices() (audio.DeviceLists, error) {
	ctx, err := initContext(false, nil)
	if err != nil {
		return audio.DeviceLists{}, fmt.Errorf("init audio context: %w", err)
	}
	defer func() {
		_ = closeContext(ctx)
	}()

	playback, err := ctx.Devices(malgo.Playback)
	if err != nil {
		return audio.DeviceLists{}, fmt.Errorf("enumerate playback devices: %w", err)
	}
	capture, err := ctx.Devices(malgo.Capture)
	if err != nil {
		return audio.DeviceLists{}, fmt.Errorf("enumerate capture devices: %w", err)
	}

	loopbackNote := "not available through miniaudio/CoreAudio on this platform"
	if runtime.GOOS == "windows" {
		loopbackNote = "supported through WASAPI; choose a playback endpoint as the loopback source"
	}

	return audio.DeviceLists{
		Playback:     mapDeviceInfos(playback, playbackLoopbackNote()),
		Capture:      mapDeviceInfos(capture, ""),
		LoopbackNote: loopbackNote,
	}, nil
}

func (b *Backend) deviceIDForSelector(kind malgo.DeviceType, source audio.Source, selector string, verbose bool, logger logging.Logger) (*malgo.DeviceID, audio.DeviceInfo, error) {
	if source == audio.SourceLoopback {
		if runtime.GOOS != "windows" {
			return nil, audio.DeviceInfo{}, fmt.Errorf("loopback device selection is only supported on Windows")
		}
		kind = malgo.Playback
	}
	devices, err := b.enumerateDeviceInfos(kind, verbose, logger)
	if err != nil {
		return nil, audio.DeviceInfo{}, err
	}
	infos := mapDeviceInfos(devices, "")
	kindName := deviceKindName(kind)
	selected, err := audio.SelectDeviceBySelector(selector, infos, kindName)
	if err != nil {
		return nil, audio.DeviceInfo{}, err
	}
	id := devices[selected.Index].ID
	return &id, selected, nil
}

func (b *Backend) enumerateDeviceInfos(kind malgo.DeviceType, verbose bool, logger logging.Logger) ([]malgo.DeviceInfo, error) {
	ctx, err := initContext(verbose, logger)
	if err != nil {
		return nil, fmt.Errorf("init audio context: %w", err)
	}
	defer func() {
		_ = closeContext(ctx)
	}()

	devices, err := ctx.Devices(kind)
	if err != nil {
		return nil, fmt.Errorf("enumerate %s devices: %w", deviceKindName(kind), err)
	}
	return devices, nil
}

func mapDeviceInfos(infos []malgo.DeviceInfo, note string) []audio.DeviceInfo {
	out := make([]audio.DeviceInfo, 0, len(infos))
	for i := range infos {
		name := infos[i].Name()
		if name == "" {
			name = "(unnamed device)"
		}
		out = append(out, audio.DeviceInfo{
			Index: i,
			Name:  name,
			ID:    infos[i].ID.String(),
			Note:  note,
		})
	}
	return out
}

func deviceKindName(kind malgo.DeviceType) string {
	switch kind {
	case malgo.Playback:
		return "playback"
	case malgo.Capture:
		return "capture"
	default:
		return "audio"
	}
}

func playbackLoopbackNote() string {
	if runtime.GOOS == "windows" {
		return "usable as WASAPI loopback source"
	}
	return ""
}

var _ = strconv.Itoa
var _ = strings.ToLower

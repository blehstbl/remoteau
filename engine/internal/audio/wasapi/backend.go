//go:build windows

package wasapi

import (
	"fmt"

	"remote-au/internal/audio"
	"remote-au/internal/logging"
)

func init() {
	audio.RegisterBackend("wasapi", func() audio.Backend { return &Backend{} })
}

// Backend is the pure-Go WASAPI audio backend (Windows only, cgo-free).
type Backend struct{}

func (b *Backend) Name() string { return "wasapi" }

func (b *Backend) SupportsLoopback() bool { return true }

// EnumerateDevices lists active render and capture endpoints.
func (b *Backend) EnumerateDevices() (audio.DeviceLists, error) {
	if err := coInitialize(); err != nil {
		return audio.DeviceLists{}, err
	}
	defer coUninitialize()

	enum, err := newMMDeviceEnumerator()
	if err != nil {
		return audio.DeviceLists{}, err
	}
	defer enum.release()

	renderDevs, err := enum.enumAudioEndpoints(dataFlowRender)
	if err != nil {
		return audio.DeviceLists{}, fmt.Errorf("enumerate render endpoints: %w", err)
	}
	captureDevs, err := enum.enumAudioEndpoints(dataFlowCapture)
	if err != nil {
		return audio.DeviceLists{}, fmt.Errorf("enumerate capture endpoints: %w", err)
	}

	lists := audio.DeviceLists{
		Playback:     mapEndpointInfos(renderDevs, "usable as WASAPI loopback source"),
		Capture:      mapEndpointInfos(captureDevs, ""),
		LoopbackNote: "supported through WASAPI; choose a playback endpoint as the loopback source",
	}
	for _, d := range renderDevs {
		d.release()
	}
	for _, d := range captureDevs {
		d.release()
	}
	return lists, nil
}

func mapEndpointInfos(devices []*mmDevice, note string) []audio.DeviceInfo {
	out := make([]audio.DeviceInfo, 0, len(devices))
	for i, d := range devices {
		name := d.friendlyName()
		if name == "" {
			name = "(unnamed device)"
		}
		id, _ := d.id()
		out = append(out, audio.DeviceInfo{
			Index: i,
			Name:  name,
			ID:    id,
			Note:  note,
		})
	}
	return out
}

// DefaultRenderEndpointID returns the id of the default render endpoint, for
// change detection by the device monitor.
func (b *Backend) DefaultRenderEndpointID() (string, error) {
	if err := coInitialize(); err != nil {
		return "", err
	}
	defer coUninitialize()

	enum, err := newMMDeviceEnumerator()
	if err != nil {
		return "", err
	}
	defer enum.release()

	dev, err := enum.getDefaultAudioEndpoint(dataFlowRender, roleConsole)
	if err != nil {
		return "", err
	}
	defer dev.release()
	return dev.id()
}

var _ = logging.Nop

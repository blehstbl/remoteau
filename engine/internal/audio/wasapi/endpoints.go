//go:build windows

package wasapi

import (
	"fmt"

	"remote-au/internal/audio"
)

// selectRenderEndpoint resolves a playback endpoint by selector, or the
// default render endpoint when the selector is empty. The returned device is
// owned by the caller (must be released).
func selectRenderEndpoint(enum *mmDeviceEnumerator, selector string) (*mmDevice, error) {
	if selector == "" {
		return enum.getDefaultAudioEndpoint(dataFlowRender, roleConsole)
	}
	devs, err := enum.enumAudioEndpoints(dataFlowRender)
	if err != nil {
		return nil, err
	}
	return pickEndpoint(devs, selector, "playback")
}

// selectCaptureEndpoint resolves a capture endpoint by selector, or the
// default capture endpoint when the selector is empty.
func selectCaptureEndpoint(enum *mmDeviceEnumerator, selector string) (*mmDevice, error) {
	if selector == "" {
		return enum.getDefaultAudioEndpoint(dataFlowCapture, roleConsole)
	}
	devs, err := enum.enumAudioEndpoints(dataFlowCapture)
	if err != nil {
		return nil, err
	}
	return pickEndpoint(devs, selector, "capture")
}

func pickEndpoint(devs []*mmDevice, selector, kind string) (*mmDevice, error) {
	infos := mapEndpointInfos(devs, "")
	selected, err := audio.SelectDeviceBySelector(selector, infos, kind)
	if err != nil {
		for _, d := range devs {
			d.release()
		}
		return nil, err
	}
	chosen := devs[selected.Index]
	for i, d := range devs {
		if i != selected.Index {
			d.release()
		}
	}
	if chosen == nil {
		return nil, fmt.Errorf("%s endpoint %q not found", kind, selector)
	}
	return chosen, nil
}

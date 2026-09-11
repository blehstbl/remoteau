package engine

import (
	"testing"

	"remote-au/internal/audio"
	"remote-au/internal/logging"
	"remote-au/internal/protocol/v2"
)

// listingBackend exposes a fixed playback list so SET_SOURCE device
// validation can be exercised without real audio endpoints.
type listingBackend struct {
	format audio.Format
}

func (b *listingBackend) Name() string           { return "listing" }
func (b *listingBackend) SupportsLoopback() bool { return true }
func (b *listingBackend) EnumerateDevices() (audio.DeviceLists, error) {
	return audio.DeviceLists{
		Playback: []audio.DeviceInfo{
			{Index: 0, Name: "Speakers (Realtek Audio)"},
			{Index: 1, Name: "HDMI Output"},
		},
	}, nil
}
func (b *listingBackend) OpenCapture(opts audio.CaptureOptions) (audio.Capture, error) {
	return &fakeCapture{format: opts.Format}, nil
}
func (b *listingBackend) OpenProcessLoopback(pids []uint32, exclude bool, format audio.Format, logger logging.Logger) (audio.Capture, error) {
	return &fakeCapture{format: format}, nil
}
func (b *listingBackend) OpenPlayback(audio.PlaybackOptions) (audio.Playback, error) {
	return nil, audio.ErrNoBackend
}

func TestQualityModeToBounds(t *testing.T) {
	tests := []struct {
		mode       uint8
		minB, maxB int
		fecBias    bool
		apply      bool
	}{
		{protocolv2.QualityModeAuto, 32000, 256000, false, true},
		{protocolv2.QualityModeLowestLatency, 32000, 64000, false, true},
		{protocolv2.QualityModeLossless, 64000, 256000, false, true},
		{protocolv2.QualityModeRobust, 32000, 128000, true, true},
		{protocolv2.QualityModeAdvanced, 0, 0, false, false},
	}
	for _, tt := range tests {
		minB, maxB, fecBias, apply := qualityModeToBounds(tt.mode)
		if minB != tt.minB || maxB != tt.maxB || fecBias != tt.fecBias || apply != tt.apply {
			t.Fatalf("mode %d: got (%d,%d,%v,%v), want (%d,%d,%v,%v)",
				tt.mode, minB, maxB, fecBias, apply, tt.minB, tt.maxB, tt.fecBias, tt.apply)
		}
	}
}

func TestQualityAndStateNames(t *testing.T) {
	if got := qualityModeName(protocolv2.QualityModeLowestLatency); got != "lowest-latency" {
		t.Fatalf("quality mode name: %q", got)
	}
	if got := qualityModeName(9); got != "" {
		t.Fatalf("unset quality mode name should be empty, got %q", got)
	}
	if got := receiverStateName(protocolv2.ReceiverStatePlaying); got != "playing" {
		t.Fatalf("receiver state name: %q", got)
	}
	if got := receiverStateName(9); got != "" {
		t.Fatalf("unset receiver state name should be empty, got %q", got)
	}
}

// TestApplySetSource: the host applies and validates receiver source
// requests (default/render-by-name/test tone).
func TestApplySetSource(t *testing.T) {
	format := audioTestFormat()
	host, err := NewHost(HostOptions{
		Name: "SRC-PC", Store: &memoryStore{}, Backend: &listingBackend{format: format},
		Format: format, Logger: loggingNop(),
	})
	if err != nil {
		t.Fatalf("host: %v", err)
	}

	// Kind 0: system default (loopback, no device selector).
	ok, detail := host.applySetSource(protocolv2.SetSource{Kind: protocolv2.SetSourceDefault})
	if !ok || detail == "" {
		t.Fatalf("default source: ok=%v detail=%q", ok, detail)
	}
	if host.opts.DeviceSelector != "" || host.opts.CaptureSource != audio.SourceLoopback {
		t.Fatalf("default source not applied: selector=%q source=%v", host.opts.DeviceSelector, host.opts.CaptureSource)
	}

	// Kind 1: case-insensitive substring match against playback devices.
	ok, detail = host.applySetSource(protocolv2.SetSource{Kind: protocolv2.SetSourceDevice, Name: "realtek"})
	if !ok {
		t.Fatalf("named source rejected: %q", detail)
	}
	if host.opts.DeviceSelector != "Speakers (Realtek Audio)" {
		t.Fatalf("named selector not applied: %q", host.opts.DeviceSelector)
	}

	// Kind 1 with a name matching nothing must be rejected.
	ok, _ = host.applySetSource(protocolv2.SetSource{Kind: protocolv2.SetSourceDevice, Name: "nonexistent"})
	if ok {
		t.Fatal("unknown device name accepted")
	}

	// Kind 2: synthetic test tone selects the sine generator.
	ok, detail = host.applySetSource(protocolv2.SetSource{Kind: protocolv2.SetSourceTestTone})
	if !ok {
		t.Fatalf("test tone rejected: %q", detail)
	}
	if host.opts.CaptureSource != audio.SourceTestTone || host.opts.ToneMode != audio.ToneSine {
		t.Fatalf("test tone not applied: source=%v tone=%v", host.opts.CaptureSource, host.opts.ToneMode)
	}

	// Unknown kinds are rejected.
	if ok, _ := host.applySetSource(protocolv2.SetSource{Kind: 9}); ok {
		t.Fatal("unknown source kind accepted")
	}
}

func TestReceiverStateForLocal(t *testing.T) {
	tests := []struct {
		state string
		want  uint8
		ok    bool
	}{
		{"connecting", protocolv2.ReceiverStateConnecting, true},
		{"streaming", protocolv2.ReceiverStatePlaying, true},
		{"reconnecting (250ms)", protocolv2.ReceiverStateReconnecting, true},
		{"idle", protocolv2.ReceiverStateStopped, true},
		{"stopped", protocolv2.ReceiverStateStopped, true},
		{"error: dial failed", 0, false},
	}
	for _, tt := range tests {
		got, ok := receiverStateForLocal(tt.state)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Fatalf("state %q: got (%d,%v), want (%d,%v)", tt.state, got, ok, tt.want, tt.ok)
		}
	}
}

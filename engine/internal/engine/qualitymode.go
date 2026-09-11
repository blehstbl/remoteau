package engine

import (
	"fmt"
	"strings"

	"remote-au/internal/audio"
	"remote-au/internal/protocol/v2"
)

// qualityModeToBounds maps a receiver quality-mode preset to host controller
// bitrate bounds and the FEC policy bias. apply reports whether the host
// should rebuild its adaptive controller: Advanced mode leaves the host's
// current tuning untouched (receiver manages its own buffering/latency).
func qualityModeToBounds(mode uint8) (minBitrate, maxBitrate int, fecBias bool, apply bool) {
	switch mode {
	case protocolv2.QualityModeAuto:
		return 32000, 256000, false, true
	case protocolv2.QualityModeLowestLatency:
		return 32000, 64000, false, true
	case protocolv2.QualityModeLossless:
		return 64000, 256000, false, true
	case protocolv2.QualityModeRobust:
		return 32000, 128000, true, true
	default: // Advanced (invalid modes are rejected at decode time).
		return 0, 0, false, false
	}
}

// qualityModeName renders a quality mode for UIs ("" when unset).
func qualityModeName(mode uint8) string {
	switch mode {
	case protocolv2.QualityModeAuto:
		return "auto"
	case protocolv2.QualityModeLowestLatency:
		return "lowest-latency"
	case protocolv2.QualityModeLossless:
		return "lossless"
	case protocolv2.QualityModeRobust:
		return "robust"
	case protocolv2.QualityModeAdvanced:
		return "advanced"
	default:
		return ""
	}
}

// receiverStateName renders a receiver state for UIs ("" when unset).
func receiverStateName(state uint8) string {
	switch state {
	case protocolv2.ReceiverStateConnecting:
		return "connecting"
	case protocolv2.ReceiverStatePairing:
		return "pairing"
	case protocolv2.ReceiverStateBuffering:
		return "buffering"
	case protocolv2.ReceiverStatePlaying:
		return "playing"
	case protocolv2.ReceiverStateInterrupted:
		return "interrupted"
	case protocolv2.ReceiverStateReconnecting:
		return "reconnecting"
	case protocolv2.ReceiverStateStopped:
		return "stopped"
	default:
		return ""
	}
}

// applySetSource performs a receiver-requested capture-source switch and
// returns (ok, detail) for the SET_SOURCE_ACK reply.
func (h *Host) applySetSource(src protocolv2.SetSource) (bool, string) {
	switch src.Kind {
	case protocolv2.SetSourceDefault:
		// Follow the system default render endpoint (loopback).
		if err := h.SetSourceDevice(""); err != nil {
			return false, err.Error()
		}
		if err := h.SetCaptureSource(audio.SourceLoopback); err != nil {
			return false, err.Error()
		}
		// SetSourceDevice/SetCaptureSource close the shared capture; reopen it
		// so the running stream keeps producing audio after the switch.
		if err := h.ensureCapture(); err != nil {
			return false, err.Error()
		}
		return true, "source: system default"

	case protocolv2.SetSourceDevice:
		// Validate the requested playback device against the backend's
		// enumerated list (same selector rules as the CLI --device flag).
		if h.opts.Backend == nil {
			return false, "host has no audio backend"
		}
		lists, err := h.opts.Backend.EnumerateDevices()
		if err != nil {
			return false, err.Error()
		}
		dev, derr := audio.SelectDeviceBySelector(strings.TrimSpace(src.Name), lists.Playback, "playback")
		if derr != nil {
			return false, derr.Error()
		}
		if err := h.SetSourceDevice(dev.Name); err != nil {
			return false, err.Error()
		}
		if err := h.SetCaptureSource(audio.SourceLoopback); err != nil {
			return false, err.Error()
		}
		if err := h.ensureCapture(); err != nil {
			return false, err.Error()
		}
		return true, "source: " + dev.Name

	case protocolv2.SetSourceTestTone:
		// Synthetic diagnostics (Phase 11): sine test pattern.
		h.mu.Lock()
		h.opts.ToneMode = audio.ToneSine
		h.mu.Unlock()
		if err := h.SetCaptureSource(audio.SourceTestTone); err != nil {
			return false, err.Error()
		}
		if err := h.ensureCapture(); err != nil {
			return false, err.Error()
		}
		return true, "source: synthetic test tone"

	default:
		return false, fmt.Sprintf("unknown source kind %d", src.Kind)
	}
}

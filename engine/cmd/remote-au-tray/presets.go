package main

// presetID identifies a streaming quality preset. It is shared by the tray
// menu and the settings window so both apply identical parameters.
type presetID int

const (
	presetAuto presetID = iota
	presetLowest
	presetLossless
	presetRobust
	presetAdvanced
)

// presetBounds maps a preset to the host's adaptive-controller bitrate bounds
// and FEC policy. apply is false for Advanced, which leaves the host's current
// tuning untouched (the receiver manages its own buffering/latency). This is
// the single source of truth for the tray and the settings window.
func presetBounds(p presetID) (minBitrate, maxBitrate int, fecBias, apply bool) {
	switch p {
	case presetLowest:
		return 32000, 64000, false, true
	case presetLossless:
		return 64000, 256000, false, true
	case presetRobust:
		return 32000, 128000, true, true
	case presetAdvanced:
		return 0, 0, false, false
	default: // presetAuto
		return 32000, 256000, false, true
	}
}

// presetName renders a preset for settings.json.
func presetName(p presetID) string {
	switch p {
	case presetLowest:
		return "lowest"
	case presetLossless:
		return "lossless"
	case presetRobust:
		return "robust"
	case presetAdvanced:
		return "advanced"
	default:
		return "auto"
	}
}

// presetFromName parses a settings.json preset, defaulting to Auto.
func presetFromName(s string) presetID {
	switch s {
	case "lowest":
		return presetLowest
	case "lossless":
		return presetLossless
	case "robust":
		return presetRobust
	case "advanced":
		return presetAdvanced
	default:
		return presetAuto
	}
}

// applyPresetID applies the preset to the running host and records it so it is
// persisted and reflected in the UI.
func (a *app) applyPresetID(p presetID) {
	a.mu.Lock()
	a.preset = p
	host := a.host
	a.mu.Unlock()
	a.saveSettings()

	if host == nil {
		return
	}
	min, max, fec, apply := presetBounds(p)
	if !apply {
		return
	}
	host.SetQuality(min, max, fec)
}

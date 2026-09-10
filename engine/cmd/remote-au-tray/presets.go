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

// profileID identifies a named, user-facing profile. A profile is a preset
// plus (for the robust/efficient ones) a status hint; the tray menu and the
// settings window share profileSpecs so they stay in lockstep.
type profileID int

const (
	profileNone profileID = iota
	profileHomeLossless
	profileGamingLowest
	profileWeakRobust
	profileRemoteEfficient
)

// profileSpec describes one named profile. Preset is resolved through the same
// presetBounds helper the Quality radios use; FEC for Robust/Remote is already
// implied by presetRobust's bounds.
type profileSpec struct {
	ID     profileID
	Name   string // UI label
	Key    string // settings.json "profile" value
	Preset presetID
	Hint   string // optional persistent status-line hint
}

// profileSpecs is the single source of truth for named profiles.
var profileSpecs = []profileSpec{
	{profileHomeLossless, "Home / Lossless", "home-lossless", presetLossless, ""},
	{profileGamingLowest, "Gaming / Lowest Latency", "gaming-lowest", presetLowest, ""},
	{profileWeakRobust, "Weak Wi-Fi / Robust", "weak-robust", presetRobust, ""},
	{profileRemoteEfficient, "Remote / Efficient", "remote-efficient", presetRobust,
		"Remote / Efficient: relay mode is used for WAN (serve --relay)"},
}

// profileByID returns the spec for a profile, or the zero value if unknown.
func profileByID(id profileID) (profileSpec, bool) {
	for _, s := range profileSpecs {
		if s.ID == id {
			return s, true
		}
	}
	return profileSpec{}, false
}

// profileName renders a profile for settings.json.
func profileName(p profileID) string {
	if s, ok := profileByID(p); ok {
		return s.Key
	}
	return ""
}

// profileFromName parses a settings.json profile, defaulting to None.
func profileFromName(s string) profileID {
	for _, spec := range profileSpecs {
		if spec.Key == s {
			return spec.ID
		}
	}
	return profileNone
}

// profileHintFor returns the persistent status hint for a profile ("" when
// the profile has none).
func profileHintFor(id profileID) string {
	if spec, ok := profileByID(id); ok {
		return spec.Hint
	}
	return ""
}

// profileTooltip describes a profile for a menu item.
func profileTooltip(spec profileSpec) string {
	desc := "Apply the " + spec.Name + " profile"
	if spec.Hint != "" {
		desc += " (" + spec.Hint + ")"
	}
	return desc
}

// applyPresetID applies a raw Quality preset and clears any named profile.
func (a *app) applyPresetID(p presetID) {
	a.applyTuning(p, profileNone)
}

// applyProfileID applies a named profile: its preset bounds (and the implied
// FEC bias) plus a persistent status hint, and records the profile name.
func (a *app) applyProfileID(id profileID) {
	spec, ok := profileByID(id)
	if !ok {
		return
	}
	a.applyTuning(spec.Preset, id)
}

// applyTuning records the preset/profile pair and pushes the tuning to the
// running host, then refreshes both menu groups' check-marks.
func (a *app) applyTuning(p presetID, prof profileID) {
	hint := ""
	if spec, ok := profileByID(prof); ok {
		hint = spec.Hint
	}
	a.mu.Lock()
	a.preset = p
	a.profile = prof
	a.profileHint = hint
	host := a.host
	a.mu.Unlock()
	a.saveSettings()
	a.refreshMenuChecks()

	if host == nil {
		return
	}
	min, max, fec, apply := presetBounds(p)
	if !apply {
		return
	}
	host.SetQuality(min, max, fec)
}

// refreshMenuChecks mirrors the persisted preset/profile onto the tray menus.
func (a *app) refreshMenuChecks() {
	a.mu.Lock()
	p := a.preset
	prof := a.profile
	a.mu.Unlock()
	for id, it := range a.qualityItems {
		if id == p {
			it.Check()
		} else {
			it.Uncheck()
		}
	}
	for id, it := range a.profileItems {
		if id == prof {
			it.Check()
		} else {
			it.Uncheck()
		}
	}
}

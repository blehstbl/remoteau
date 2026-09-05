// Package quality implements RemoteAU's adaptive quality controller: it
// turns smoothed network measurements into codec bitrate, FEC and jitter
// target decisions with hysteresis so settings do not oscillate. This is the
// improved take on EchoWarp's adaptive logic (Phase 6).
package quality

import (
	"sync"
	"time"
)

// Level classifies overall network health.
type Level int

const (
	LevelExcellent Level = iota
	LevelGood
	LevelFair
	LevelPoor
)

func (l Level) String() string {
	switch l {
	case LevelExcellent:
		return "excellent"
	case LevelGood:
		return "good"
	case LevelFair:
		return "fair"
	default:
		return "poor"
	}
}

// Sample is one network measurement window (typically 1 s).
type Sample struct {
	LossPercent float64
	LatePercent float64
	JitterMs    float64
	RTTMs       float64
	BufDepthMs  float64
	Underruns   uint64
}

// Decision is what the controller recommends for this window.
type Decision struct {
	BitrateBps   int
	FECEnabled   bool
	ExpectedLoss int // opus encoder packet-loss percentage
	JitterTargetMs float64
	// SwitchToPCM/SwitchToOpus are hints used only in Auto mode when both
	// codecs are negotiated. Zero values mean "keep current codec".
	SwitchToPCM bool
	SwitchToOpus bool
	Level        Level
}

// Controller smooths inputs and applies cooldown-gated decisions.
type Controller struct {
	mu sync.Mutex

	// Configuration.
	minBitrate int
	maxBitrate int
	cooldown   time.Duration

	// State.
	bitrate     int
	fec         bool
	expectedLoss int
	lastAdjust  time.Time
	lossEWMA    float64
	lateEWMA    float64
	jitterEWMA  float64
	rttEWMA     float64
	underrunAcc uint64
	initialized bool
}

// NewController creates a controller. bitrates are clamped to sane Opus
// bounds.
func NewController(minBitrate, maxBitrate int) *Controller {
	if minBitrate <= 0 {
		minBitrate = 32000
	}
	if maxBitrate < minBitrate {
		maxBitrate = minBitrate
	}
	if maxBitrate > 510000 {
		maxBitrate = 510000
	}
	start := (minBitrate + maxBitrate) / 2
	return &Controller{
		minBitrate:  minBitrate,
		maxBitrate:  maxBitrate,
		cooldown:    2 * time.Second,
		bitrate:     start,
		expectedLoss: 0,
	}
}

// Update feeds one sample and returns the current decision. Decisions change
// at most once per cooldown period.
func (c *Controller) Update(s Sample, now time.Time) Decision {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.initialized {
		c.initialized = true
		c.lossEWMA = s.LossPercent
		c.lateEWMA = s.LatePercent
		c.jitterEWMA = s.JitterMs
		c.rttEWMA = s.RTTMs
	} else {
		const a = 0.3
		c.lossEWMA += a * (s.LossPercent - c.lossEWMA)
		c.lateEWMA += a * (s.LatePercent - c.lateEWMA)
		c.jitterEWMA += a * (s.JitterMs - c.jitterEWMA)
		c.rttEWMA += a * (s.RTTMs - c.rttEWMA)
	}
	c.underrunAcc += s.Underruns

	level := classify(c.lossEWMA, c.lateEWMA, c.jitterEWMA, c.rttEWMA)

	dec := Decision{
		BitrateBps:   c.bitrate,
		FECEnabled:   c.fec,
		ExpectedLoss: c.expectedLoss,
		JitterTargetMs: jitterTargetFor(level, c.jitterEWMA),
		Level:        level,
	}

	if now.Sub(c.lastAdjust) < c.cooldown {
		return dec
	}

	switch level {
	case LevelExcellent:
		// Scale up gently; drop FEC when things have been clean for a while.
		dec.BitrateBps = scale(c.bitrate, 1.10, c.minBitrate, c.maxBitrate)
		if c.lossEWMA < 0.3 && c.fec {
			dec.FECEnabled = false
			dec.ExpectedLoss = 0
		}
		dec.SwitchToPCM = true // clean network: prefer lossless
	case LevelGood:
		dec.BitrateBps = scale(c.bitrate, 1.0, c.minBitrate, c.maxBitrate)
	case LevelFair:
		dec.BitrateBps = scale(c.bitrate, 0.85, c.minBitrate, c.maxBitrate)
		dec.FECEnabled = true
		dec.ExpectedLoss = 10
		dec.SwitchToOpus = true
	case LevelPoor:
		dec.BitrateBps = scale(c.bitrate, 0.70, c.minBitrate, c.maxBitrate)
		dec.FECEnabled = true
		dec.ExpectedLoss = 20
		dec.SwitchToOpus = true
	}

	// Apply.
	c.bitrate = dec.BitrateBps
	c.fec = dec.FECEnabled
	c.expectedLoss = dec.ExpectedLoss
	c.lastAdjust = now
	return dec
}

// Bitrate returns the current bitrate.
func (c *Controller) Bitrate() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bitrate
}

func scale(cur int, factor float64, min, max int) int {
	next := int(float64(cur) * factor)
	if next < min {
		next = min
	}
	if next > max {
		next = max
	}
	return next
}

// classify mirrors VoIP-style thresholds, tightened for LAN audio.
func classify(loss, late, jitterMs, rttMs float64) Level {
	switch {
	case loss < 0.5 && late < 0.5 && jitterMs < 6 && rttMs < 20:
		return LevelExcellent
	case loss < 2 && late < 2 && jitterMs < 20 && rttMs < 60:
		return LevelGood
	case loss < 6 && late < 6 && jitterMs < 50 && rttMs < 150:
		return LevelFair
	default:
		return LevelPoor
	}
}

// jitterTargetFor maps the level to a receiver jitter target in ms.
func jitterTargetFor(level Level, jitterMs float64) float64 {
	switch level {
	case LevelExcellent:
		return 12
	case LevelGood:
		return 20
	case LevelFair:
		return 45
	default:
		return 90
	}
}

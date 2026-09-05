package audio

import (
	"fmt"
	"math"
	"sync"
	"time"

	"remote-au/internal/logging"
)

// SourceTestTone is a synthetic capture source (Phase 11 diagnostic): it
// generates deterministic audio inside the engine and bypasses WASAPI
// entirely, so the full product path (engine → codec → transport → receiver)
// can be validated on a Windows machine with no active render endpoint.
//
// Modes:
//   - Tone: 440 Hz sine at a safe level (-12 dBFS) with a slow stereo pan.
//   - Click: one short full-scale pulse per second (latency checking).
const (
	SourceTestTone Source = iota + 100 // offset: avoid colliding with future built-ins
)

// ToneMode selects the diagnostic pattern.
type ToneMode int

const (
	ToneSine ToneMode = iota
	ToneClick
)

// SyntheticCapture implements Capture with generated audio.
type SyntheticCapture struct {
	format   Format
	mode     ToneMode
	ring     *Ring
	stop     chan struct{}
	stopOnce sync.Once
	logger   logging.Logger
}

// NewSyntheticCapture creates a started synthetic source. frameDuration is
// ignored; the generator produces 5 ms chunks.
func NewSyntheticCapture(format Format, mode ToneMode, logger logging.Logger) (*SyntheticCapture, error) {
	if err := format.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = logging.Nop()
	}
	c := &SyntheticCapture{
		format: format,
		mode:   mode,
		ring:   NewRing(format.Rate*format.BytesPerFrame(), format.BytesPerFrame()),
		stop:   make(chan struct{}),
		logger: logger,
	}
	go c.generate()
	return c, nil
}

func (c *SyntheticCapture) generate() {
	const chunkMs = 5
	frames := c.format.Rate * chunkMs / 1000
	chunk := make([]byte, frames*c.format.BytesPerFrame())
	phase := 0.0
	totalFrames := 0
	toneLevel := 0.25 // -12 dBFS
	panPhase := 0.0

	ticker := time.NewTicker(time.Duration(chunkMs) * time.Millisecond)
	defer ticker.Stop()
	start := time.Now()

	c.logger.Infof("synthetic test source started: %s, %d Hz %d ch", toneModeName(c.mode), c.format.Rate, c.format.Channels)
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
		}

		ch := c.format.Channels
		clickThisChunk := false
		if c.mode == ToneClick {
			// One 2 ms pulse at each whole second.
			elapsed := time.Since(start).Seconds()
			if int(elapsed*1000)%1000 < 2 {
				clickThisChunk = true
			}
		}

		for f := 0; f < frames; f++ {
			totalFrames++
			var sample float64
			switch {
			case c.mode == ToneClick:
				sample = 0.0
				if clickThisChunk && f < c.format.Rate*2/1000 {
					sample = 0.9 * math.Sin(2*math.Pi*1000*float64(f)/float64(c.format.Rate))
				}
			default:
				phase += 2 * math.Pi * 440.0 / float64(c.format.Rate)
				if phase > 2*math.Pi {
					phase -= 2 * math.Pi
				}
				panPhase += 2 * math.Pi / float64(c.format.Rate*4) // 4 s pan cycle
				sample = toneLevel * math.Sin(phase)
				pan := (math.Sin(panPhase) + 1) / 2
				_ = pan
			}

			for chI := 0; chI < ch; chI++ {
				v := sample
				if c.mode == ToneSine && ch == 2 {
					// Slow L/R pan so both channels stay identifiable.
					pan := (math.Sin(panPhase) + 1) / 2
					if chI == 0 {
						v = sample * (1 - 0.5*pan)
					} else {
						v = sample * (0.5 + 0.5*pan)
					}
				}
				s := int16(clampInt16(v * 32767))
				off := (f*ch + chI) * 2
				chunk[off] = byte(s & 0xFF)
				chunk[off+1] = byte(uint16(s) >> 8)
			}
		}
		c.ring.TryWrite(chunk)
	}
}

func clampInt16(v float64) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}

func toneModeName(m ToneMode) string {
	if m == ToneClick {
		return "click/impulse"
	}
	return "440 Hz tone"
}

func (c *SyntheticCapture) Format() Format { return c.format }

func (c *SyntheticCapture) Read(dst []byte) int { return c.ring.Read(dst) }

func (c *SyntheticCapture) Start() error { return nil } // already generating

func (c *SyntheticCapture) Close() error {
	c.stopOnce.Do(func() { close(c.stop) })
	return nil
}

// Ensure SourceTestTone is a valid capture source label.
func (s Source) String() string {
	switch s {
	case SourceMicrophone:
		return "mic"
	case SourceLoopback:
		return "loopback"
	case SourceTestTone:
		return "testtone"
	default:
		return fmt.Sprintf("source(%d)", int(s))
	}
}

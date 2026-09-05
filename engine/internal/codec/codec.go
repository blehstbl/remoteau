// Package codec abstracts the audio codec used on the wire (Phase 6): raw
// PCM passthrough (lossless, default) and Opus (compressed, FEC/DTX/PLC) —
// the Opus implementation is behind the `opus` build tag because it links
// libopus via cgo.
package codec

import (
	"fmt"

	"remote-au/internal/logging"
)

// Codec converts between PCM S16LE and wire frames.
//
// Contract:
//   - EncodeFrame takes exactly FrameBytes() PCM and returns the wire frame
//     (PCM passthrough returns the input copied; Opus returns one packet).
//   - DecodeFrame decodes one wire frame; `lost=true` triggers concealment
//     (Opus PLC) and the return value is the replacement PCM.
type Codec interface {
	Name() string
	Config() Config   // the configuration this codec was built with
	FrameBytes() int  // PCM bytes per encode input
	WireMTU() int     // max wire frame size
	EncodeFrame(pcm []byte) ([]byte, error)
	DecodeFrame(wire []byte, lost bool, out []byte) (int, error)
	Close() error
}

// Config describes one negotiated codec configuration.
type Config struct {
	// PCM passthrough.
	IsOpus bool

	SampleRate int
	Channels   int
	FrameMs    int

	// Opus-only settings.
	Bitrate    int
	FEC        bool
	DTX        bool
	Complexity int
	AppID      int // 0=audio 1=voip 2=lowdelay
}

// FrameSamples returns PCM frames per encode input.
func (c Config) FrameSamples() int {
	return c.SampleRate * c.FrameMs / 1000
}

// PCMFrameBytes returns the PCM byte count per frame.
func (c Config) PCMFrameBytes() int {
	return c.FrameSamples() * c.Channels * 2
}

// New returns a Codec for the config. Without the opus build tag, Opus
// configs fail with ErrOpusUnavailable and callers fall back to PCM.
func New(cfg Config, logger logging.Logger) (Codec, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.IsOpus {
		return newOpusCodec(cfg, logger)
	}
	return &pcmCodec{cfg: cfg}, nil
}

// ErrOpusUnavailable is returned when the binary lacks the opus build tag.
var ErrOpusUnavailable = fmt.Errorf("opus support not built into this binary (build with -tags opus)")

// OpusAvailable reports whether this binary was built with libopus.
// The value is defined per build-tag file.
func OpusAvailable() bool { return opusAvailable }

// Validate checks the config.
func (c Config) Validate() error {
	if c.SampleRate < 8000 || c.SampleRate > 192000 {
		return fmt.Errorf("codec: sample rate out of range: %d", c.SampleRate)
	}
	if c.Channels < 1 || c.Channels > 8 {
		return fmt.Errorf("codec: channel count out of range: %d", c.Channels)
	}
	switch c.FrameMs {
	case 2, 5, 10, 20:
	default:
		return fmt.Errorf("codec: unsupported frame duration %dms", c.FrameMs)
	}
	if c.IsOpus {
		if c.Bitrate != 0 && (c.Bitrate < 6000 || c.Bitrate > 510000) {
			return fmt.Errorf("codec: opus bitrate out of range: %d", c.Bitrate)
		}
	}
	return nil
}

// pcmCodec is the lossless passthrough.
type pcmCodec struct {
	cfg Config
}

func (p *pcmCodec) Name() string     { return "pcm" }
func (p *pcmCodec) Config() Config   { return p.cfg }
func (p *pcmCodec) FrameBytes() int  { return p.cfg.PCMFrameBytes() }
func (p *pcmCodec) WireMTU() int     { return p.cfg.PCMFrameBytes() }

func (p *pcmCodec) EncodeFrame(pcm []byte) ([]byte, error) {
	if len(pcm) != p.cfg.PCMFrameBytes() {
		return nil, fmt.Errorf("pcm encode: want %d bytes, got %d", p.cfg.PCMFrameBytes(), len(pcm))
	}
	out := make([]byte, len(pcm))
	copy(out, pcm)
	return out, nil
}

func (p *pcmCodec) DecodeFrame(wire []byte, lost bool, out []byte) (int, error) {
	if lost {
		// PCM PLC: caller (receiver) handles interpolation; return silence.
		clear(out[:min(len(out), p.cfg.PCMFrameBytes())])
		return min(len(out), p.cfg.PCMFrameBytes()), nil
	}
	if len(wire) != p.cfg.PCMFrameBytes() {
		return 0, fmt.Errorf("pcm decode: want %d bytes, got %d", p.cfg.PCMFrameBytes(), len(wire))
	}
	n := copy(out, wire)
	return n, nil
}

func (p *pcmCodec) Close() error { return nil }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

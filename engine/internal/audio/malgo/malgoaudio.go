//go:build malgo

// Package malgoaudio is the miniaudio (cgo) audio backend, adapted from the
// original remote-au audio package. Build with -tags malgo to include it.
package malgoaudio

import (
	"fmt"
	"runtime"
	"sync"

	"github.com/gen2brain/malgo"

	"remote-au/internal/audio"
	"remote-au/internal/logging"
)

func init() {
	audio.RegisterBackend("malgo", func() audio.Backend { return &Backend{} })
}

// Backend implements audio.Backend with miniaudio (WASAPI on Windows).
type Backend struct{}

func (b *Backend) Name() string { return "malgo" }

func (b *Backend) SupportsLoopback() bool { return runtime.GOOS == "windows" }

type Capture struct {
	ctx       *malgo.AllocatedContext
	dev       *malgo.Device
	format    audio.Format
	ring      *audio.Ring
	closeOnce sync.Once
	closeErr  error
}

type Playback struct {
	ctx       *malgo.AllocatedContext
	dev       *malgo.Device
	format    audio.Format
	closeOnce sync.Once
	closeErr  error
}

func (b *Backend) OpenCapture(opts audio.CaptureOptions) (audio.Capture, error) {
	format := opts.Format
	if format == (audio.Format{}) {
		format = audio.DefaultFormat()
	}
	if err := format.Validate(); err != nil {
		return nil, err
	}
	if opts.Source == audio.SourceLoopback && runtime.GOOS != "windows" {
		return nil, fmt.Errorf("loopback capture is only supported through WASAPI on Windows")
	}

	deviceType := malgo.Capture
	if opts.Source == audio.SourceLoopback {
		deviceType = malgo.Loopback
	}

	ringFrames := opts.RingFrames
	if ringFrames <= 0 {
		ringFrames = format.Rate / 4
	}
	ring := audio.NewRing(ringFrames*format.BytesPerFrame(), format.BytesPerFrame())

	ctx, err := initContext(opts.Verbose, opts.Logger)
	if err != nil {
		return nil, fmt.Errorf("init capture context: %w", err)
	}

	cfg := malgo.DefaultDeviceConfig(deviceType)
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = uint32(format.Channels)
	cfg.SampleRate = uint32(format.Rate)
	cfg.PeriodSizeInFrames = uint32(format.FrameSamples)
	cfg.PerformanceProfile = malgo.LowLatency

	if selector := opts.DeviceSelector; selector != "" {
		id, info, err := b.deviceIDForSelector(deviceType, opts.Source, selector, opts.Verbose, opts.Logger)
		if err != nil {
			_ = closeContext(ctx)
			return nil, err
		}
		cfg.Capture.DeviceID = id.Pointer()
		defer freeDeviceIDPointer(cfg.Capture.DeviceID)
		_ = info
	}

	cb := func(out, in []byte, frameCount uint32) {
		_, _ = out, frameCount
		// Copy immediately into a bounded ring; callers consume outside the audio callback.
		ring.TryWrite(in)
	}

	dev, err := malgo.InitDevice(ctx.Context, cfg, malgo.DeviceCallbacks{Data: cb})
	if err != nil {
		_ = closeContext(ctx)
		return nil, fmt.Errorf("init capture device: %w", err)
	}

	return &Capture{
		ctx:    ctx,
		dev:    dev,
		format: format,
		ring:   ring,
	}, nil
}

func (c *Capture) Start() error {
	if err := c.dev.Start(); err != nil {
		return fmt.Errorf("start capture device: %w", err)
	}
	return nil
}

func (c *Capture) Close() error {
	c.closeOnce.Do(func() {
		if c.dev != nil {
			c.dev.Uninit()
		}
		c.closeErr = closeContext(c.ctx)
	})
	if c.closeErr != nil {
		return fmt.Errorf("close capture context: %w", c.closeErr)
	}
	return nil
}

func (c *Capture) Read(dst []byte) int {
	return c.ring.Read(dst)
}

func (c *Capture) Format() audio.Format {
	return c.format
}

func (b *Backend) OpenPlayback(opts audio.PlaybackOptions) (audio.Playback, error) {
	format := opts.Format
	if format == (audio.Format{}) {
		format = audio.DefaultFormat()
	}
	if err := format.Validate(); err != nil {
		return nil, err
	}
	pull := opts.Pull
	if pull == nil {
		pull = func(out []byte, _ uint32) {
			clear(out)
		}
	}

	ctx, err := initContext(opts.Verbose, opts.Logger)
	if err != nil {
		return nil, fmt.Errorf("init playback context: %w", err)
	}

	cfg := malgo.DefaultDeviceConfig(malgo.Playback)
	cfg.Playback.Format = malgo.FormatS16
	cfg.Playback.Channels = uint32(format.Channels)
	cfg.SampleRate = uint32(format.Rate)
	cfg.PeriodSizeInFrames = uint32(format.FrameSamples)
	cfg.PerformanceProfile = malgo.LowLatency

	if selector := opts.DeviceSelector; selector != "" {
		id, info, err := b.deviceIDForSelector(malgo.Playback, audio.SourceMicrophone, selector, opts.Verbose, opts.Logger)
		if err != nil {
			_ = closeContext(ctx)
			return nil, err
		}
		cfg.Playback.DeviceID = id.Pointer()
		defer freeDeviceIDPointer(cfg.Playback.DeviceID)
		_ = info
	}

	bytesPerFrame := format.BytesPerFrame()
	cb := func(out, in []byte, frameCount uint32) {
		_ = in
		frames := frameCount
		wantBytes := int(frameCount) * bytesPerFrame
		if wantBytes > len(out) {
			frames = uint32(len(out) / bytesPerFrame)
			wantBytes = int(frames) * bytesPerFrame
		}
		out = out[:wantBytes]
		clear(out)
		// frameCount is the audio clock; PeriodSizeInFrames is only a latency hint.
		pull(out, frames)
	}

	dev, err := malgo.InitDevice(ctx.Context, cfg, malgo.DeviceCallbacks{Data: cb})
	if err != nil {
		_ = closeContext(ctx)
		return nil, fmt.Errorf("init playback device: %w", err)
	}

	return &Playback{
		ctx:    ctx,
		dev:    dev,
		format: format,
	}, nil
}

func (p *Playback) Start() error {
	if err := p.dev.Start(); err != nil {
		return fmt.Errorf("start playback device: %w", err)
	}
	return nil
}

func (p *Playback) Close() error {
	p.closeOnce.Do(func() {
		if p.dev != nil {
			p.dev.Uninit()
		}
		p.closeErr = closeContext(p.ctx)
	})
	if p.closeErr != nil {
		return fmt.Errorf("close playback context: %w", p.closeErr)
	}
	return nil
}

func (p *Playback) Format() audio.Format {
	return p.format
}

func initContext(debug bool, logger logging.Logger) (*malgo.AllocatedContext, error) {
	if logger == nil {
		logger = logging.Nop()
	}
	var logProc malgo.LogProc
	if debug {
		logProc = func(message string) {
			logger.Debugf("malgo: %s", message)
		}
	}
	return malgo.InitContext(nil, malgo.ContextConfig{}, logProc)
}

func closeContext(ctx *malgo.AllocatedContext) error {
	if ctx == nil {
		return nil
	}
	err := ctx.Uninit()
	ctx.Free()
	return err
}

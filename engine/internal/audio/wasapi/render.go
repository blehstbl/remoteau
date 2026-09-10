//go:build windows

package wasapi

import (
	"encoding/binary"
	"fmt"
	"math"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"remote-au/internal/audio"
	"remote-au/internal/logging"
)

// renderLatencyMS is the target render buffer latency.
const renderLatencyMS = 30

// Playback implements audio.Playback over WASAPI shared-mode render.
type Playback struct {
	format    audio.Format
	closeOnce sync.Once
	logger    logging.Logger

	client       *audioClient
	render       *renderClient
	bufferFrames uint32

	deviceBytesPerFrame int
	deviceRate          int
	deviceChannels      int
	deviceIsFloat       bool
	needsResample       bool
	resampler           *s16Resampler
	running             chan struct{}
	stopOnce            sync.Once
	stopped             chan struct{}
}

func (b *Backend) OpenPlayback(opts audio.PlaybackOptions) (audio.Playback, error) {
	if err := opts.Format.Validate(); err != nil {
		return nil, err
	}
	if opts.Pull == nil {
		return nil, fmt.Errorf("playback pull function is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = logging.Nop()
	}

	if err := coInitialize(); err != nil {
		return nil, err
	}
	// COM is per-thread: keep it initialized on THIS thread until setup is
	// done (the render goroutine initializes its own apartment separately).
	defer coUninitialize()

	enum, err := newMMDeviceEnumerator()
	if err != nil {
		return nil, err
	}
	defer enum.release()

	var dev *mmDevice
	if opts.DeviceSelector == "" {
		dev, err = enum.getDefaultAudioEndpoint(dataFlowRender, roleConsole)
	} else {
		dev, err = selectRenderEndpoint(enum, opts.DeviceSelector)
	}
	if err != nil {
		return nil, err
	}
	defer dev.release()

	obj, err := dev.activate(iidIAudioClient)
	if err != nil {
		return nil, err
	}
	client := &audioClient{obj: obj}

	mixFormat, err := client.getMixFormat()
	if err != nil {
		client.release()
		return nil, fmt.Errorf("get mix format: %w", err)
	}

	if err := client.initialize(shareModeShared, streamFlagsEventCallback|streamFlagsNoPersist, hnsMilliseconds(renderLatencyMS*2), mixFormat); err != nil {
		client.release()
		return nil, fmt.Errorf("initialize render (%s): %w", formatSummary(mixFormat), err)
	}

	bufFrames, err := client.bufferSize()
	if err != nil {
		client.release()
		return nil, err
	}

	svc, err := client.getService(iidIAudioRenderClient)
	if err != nil {
		client.release()
		return nil, err
	}

	ev, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		client.release()
		return nil, fmt.Errorf("create render event: %w", err)
	}
	if err := client.setEventHandle(uintptr(ev)); err != nil {
		client.release()
		windows.CloseHandle(ev)
		return nil, err
	}

	needsResample := mixFormat.SampleRate != opts.Format.Rate
	p := &Playback{
		format:              opts.Format,
		logger:              logger,
		client:              client,
		render:              &renderClient{obj: svc},
		bufferFrames:        bufFrames,
		deviceBytesPerFrame: mixFormat.Channels * mixFormat.Bits / 8,
		deviceRate:          mixFormat.SampleRate,
		deviceChannels:      mixFormat.Channels,
		deviceIsFloat:       mixFormat.IsFloat,
		needsResample:       needsResample,
		running:             make(chan struct{}),
		stopped:             make(chan struct{}),
	}
	if needsResample {
		p.resampler = newS16Resampler(opts.Format.Rate, opts.Format.Channels, mixFormat.SampleRate)
	}

	go p.run(opts.Pull, ev, dev.friendlyName())

	return p, nil
}

// run is the render goroutine driven by the WASAPI event handle.
func (p *Playback) run(pull audio.PullFunc, ev windows.Handle, devName string) {
	runtime.LockOSThread()
	if err := coInitialize(); err != nil {
		p.logger.Errorf("wasapi render COM init: %v", err)
		return
	}
	defer coUninitialize()
	defer p.render.release()
	defer close(p.stopped)

	p.logger.Infof("wasapi playback started: %s (device %dHz %dch)", devName, p.deviceRate, p.deviceChannels)

	if err := p.client.start(); err != nil {
		p.logger.Errorf("wasapi render start: %v", err)
		return
	}
	defer p.client.stop()

	deviceBytesPerFrame := p.deviceBytesPerFrame
	// `out` holds engine-format S16LE (the resampler emits device-rate S16 in
	// the same channel layout), so size it by the engine frame size.
	out := make([]byte, int(p.bufferFrames)*p.format.BytesPerFrame()+64)

	for {
		select {
		case <-p.running:
			return
		default:
		}

		wo, err := windows.WaitForSingleObject(ev, 500)
		if err != nil {
			p.logger.Errorf("wasapi wait: %v", err)
			return
		}
		if wo == uint32(windows.WAIT_TIMEOUT) {
			continue
		}

		padding, err := p.client.currentPadding()
		if err != nil {
			p.logger.Errorf("wasapi padding: %v", err)
			return
		}
		if padding >= p.bufferFrames {
			continue
		}
		framesToWrite := int(p.bufferFrames - padding)
		if framesToWrite == 0 {
			continue
		}

		n := p.writeFrames(pull, out, framesToWrite)
		if n == 0 {
			data, err := p.render.getBuffer(uint32(framesToWrite))
			if err != nil {
				p.logger.Errorf("wasapi render getbuffer: %v", err)
				return
			}
			clear(unsafe.Slice((*byte)(data), framesToWrite*deviceBytesPerFrame))
			p.render.releaseBuffer(uint32(framesToWrite), bufferFlagsSilent)
			continue
		}

		data, err := p.render.getBuffer(uint32(n))
		if err != nil {
			p.logger.Errorf("wasapi render getbuffer: %v", err)
			return
		}
		buf := unsafe.Slice((*byte)(data), n*deviceBytesPerFrame)
		// `out` holds n device-rate frames of S16LE PCM in the ENGINE channel
		// layout; map to the device channel layout (safe for mismatches).
		src := out[:n*p.format.BytesPerFrame()]
		if p.deviceIsFloat {
			s16ToF32Mapped(buf, src, p.format.Channels, p.deviceChannels)
		} else {
			s16ToS16Mapped(buf, src, p.format.Channels, p.deviceChannels)
		}
		p.render.releaseBuffer(uint32(n), 0)
	}
}

// writeFrames fills out[0 : framesToWrite*deviceBytesPerFrame] and returns the
// number of device frames written.
func (p *Playback) writeFrames(pull audio.PullFunc, out []byte, framesToWrite int) int {
	if !p.needsResample {
		bytesPerFrame := p.format.BytesPerFrame()
		wrote := 0
		for wrote < framesToWrite {
			n := pull(out[wrote*bytesPerFrame:framesToWrite*bytesPerFrame], uint32(framesToWrite-wrote))
			if n == 0 {
				clear(out[wrote*bytesPerFrame : framesToWrite*bytesPerFrame])
				return framesToWrite
			}
			wrote += n / bytesPerFrame
		}
		return framesToWrite
	}
	return p.resampler.pull(pull, out, framesToWrite)
}

func (p *Playback) Format() audio.Format { return p.format }

func (p *Playback) Start() error { return nil } // started by the render goroutine

func (p *Playback) Close() error {
	p.stopOnce.Do(func() {
		close(p.running)
		if p.client != nil {
			p.client.stop()
		}
		<-p.stopped
		if p.client != nil {
			p.client.release()
			p.client = nil
		}
	})
	return nil
}

// s16Resampler is a streaming linear-interpolation resampler from an
// interleaved S16 source at inRate to a device at outRate, channel-preserving.
type s16Resampler struct {
	inRate, outRate int
	channels        int

	pending  []int16 // interleaved, at inRate
	pos      float64 // fractional read position into pending
	inScale  float64
	outScale float64
}

func newS16Resampler(inRate, channels, outRate int) *s16Resampler {
	return &s16Resampler{
		inRate:   inRate,
		outRate:  outRate,
		channels: channels,
	}
}

// pull produces wantFrames device frames, sourcing the engine through pull().
// It never returns partial frames: silence is substituted on starvation and
// resampler state resets so recovery is glitch-free.
func (r *s16Resampler) pull(pull audio.PullFunc, out []byte, wantFrames int) int {
	ch := r.channels
	tmp := make([]byte, 4096*ch*2)
	outFrames := 0
	wb := 0
	ratio := float64(r.inRate) / float64(r.outRate)

	for outFrames < wantFrames {
		// Ensure enough input: need int(pos)+2 frames of pending.
		for len(r.pending)/ch < int(r.pos)+2 {
			n := pull(tmp, uint32(len(tmp)/(ch*2)))
			if n == 0 {
				// Starvation: emit silence for the remainder and reset state.
				clear(out[outFrames*ch*2 : wantFrames*ch*2])
				r.pending = r.pending[:0]
				r.pos = 0
				return wantFrames
			}
			s := bytesToInt16(tmp[:n])
			r.pending = append(r.pending, s...)
		}

		i := int(r.pos)
		t := r.pos - float64(i)
		for c := 0; c < ch; c++ {
			a := float64(r.pending[i*ch+c])
			b := float64(r.pending[(i+1)*ch+c])
			v := a + (b-a)*t
			binary.LittleEndian.PutUint16(out[wb:], s16FromF64(v/32768.0))
			wb += 2
		}
		outFrames++
		r.pos += ratio
		// Consume pending frames fully passed.
		if consumed := int(r.pos); consumed > 0 {
			r.pending = r.pending[consumed*ch:]
			r.pos -= float64(consumed)
		}
	}
	return outFrames
}

func bytesToInt16(b []byte) []int16 {
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return out
}

func s16FromF64(v float64) uint16 {
	if v >= 1.0 {
		return 32767
	}
	if v <= -1.0 {
		return 32768
	}
	return uint16(int16(math.Round(v * 32767)))
}

// s16ToF32Mapped converts interleaved S16LE (srcCh channels) to interleaved
// float32 (dstCh channels), duplicating mono and zero-filling extra device
// channels. dst must hold nFrames*dstCh floats.
func s16ToF32Mapped(dst, src []byte, srcCh, dstCh int) {
	if srcCh <= 0 || dstCh <= 0 {
		return
	}
	nFrames := len(src) / (srcCh * 2)
	out := unsafe.Slice((*float32)(unsafe.Pointer(&dst[0])), nFrames*dstCh)
	for f := 0; f < nFrames; f++ {
		for c := 0; c < dstCh; c++ {
			var v float32
			switch {
			case c < srcCh:
				s := int16(binary.LittleEndian.Uint16(src[(f*srcCh+c)*2:]))
				v = float32(s) / 32768.0
			case srcCh == 1:
				s := int16(binary.LittleEndian.Uint16(src[f*2:]))
				v = float32(s) / 32768.0
			}
			out[f*dstCh+c] = v
		}
	}
}

// s16ToS16Mapped copies/maps interleaved S16LE between channel layouts.
func s16ToS16Mapped(dst, src []byte, srcCh, dstCh int) {
	if srcCh <= 0 || dstCh <= 0 {
		return
	}
	nFrames := len(src) / (srcCh * 2)
	for f := 0; f < nFrames; f++ {
		for c := 0; c < dstCh; c++ {
			var s int16
			switch {
			case c < srcCh:
				s = int16(binary.LittleEndian.Uint16(src[(f*srcCh+c)*2:]))
			case srcCh == 1:
				s = int16(binary.LittleEndian.Uint16(src[f*2:]))
			}
			binary.LittleEndian.PutUint16(dst[(f*dstCh+c)*2:], uint16(s))
		}
	}
}

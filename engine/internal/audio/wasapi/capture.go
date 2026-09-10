//go:build windows

package wasapi

import (
	"encoding/binary"
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"remote-au/internal/audio"
	"remote-au/internal/logging"
)

const (
	// capturePollInterval is the idle wait between GetNextPacketSize polls.
	// Event-driven mode is not supported for loopback capture, so we poll.
	capturePollInterval = 2 * time.Millisecond
	// captureBuffer100ns is the shared-mode buffer duration (20 ms).
	captureBuffer100ns = uint64(2_000_000) // 200ms in 100ns units? no: 1ms = 10_000
)

func hnsMilliseconds(ms uint64) uint64 { return ms * 10_000 }

// Capture implements audio.Capture over WASAPI (loopback or microphone).
// Read returns S16LE interleaved PCM converted from the device mix format.
type Capture struct {
	format    audio.Format
	ring      *audio.Ring
	closeOnce sync.Once
	logger    logging.Logger

	client   *audioClient
	done     chan struct{}
	gone     chan struct{}
	stopOnce sync.Once
}

// OpenCapture opens a capture stream. For SourceLoopback it opens the render
// endpoint named by DeviceSelector (or the default) in loopback mode.
func (b *Backend) OpenCapture(opts audio.CaptureOptions) (audio.Capture, error) {
	if err := opts.Format.Validate(); err != nil {
		return nil, err
	}

	if err := coInitialize(); err != nil {
		return nil, err
	}
	// COM is per-thread; the capture goroutine initializes its own apartment.
	coUninitialize()

	enum, err := newMMDeviceEnumerator()
	if err != nil {
		return nil, err
	}
	defer enum.release()

	var dev *mmDevice
	switch opts.Source {
	case audio.SourceLoopback:
		dev, err = selectRenderEndpoint(enum, opts.DeviceSelector)
	case audio.SourceMicrophone:
		dev, err = selectCaptureEndpoint(enum, opts.DeviceSelector)
	default:
		err = fmt.Errorf("unknown capture source %d", opts.Source)
	}
	if err != nil {
		return nil, err
	}
	defer dev.release()

	flags := streamFlagsNoPersist
	if opts.Source == audio.SourceLoopback {
		flags |= streamFlagsLoopback
	}

	audioClientObj, err := dev.activate(iidIAudioClient)
	if err != nil {
		return nil, err
	}
	devID, _ := dev.id()
	devName := dev.friendlyName()
	return newPacketCapture(audioClientObj, flags, opts, devName, devID)
}

// newPacketCapture finishes configuring an activated IAudioClient for the
// packet-poll capture loop: mix format → initialize → capture service →
// converter → ring → poll goroutine. Shared by OpenCapture (endpoint
// activation) and OpenProcessLoopback (ActivateAudioInterfaceAsync). It owns
// clientObj: released on error, moved into the returned Capture otherwise.
func newPacketCapture(clientObj unsafe.Pointer, streamFlags int, opts audio.CaptureOptions, devName, devID string) (*Capture, error) {
	client := &audioClient{obj: clientObj}
	mixFormat, err := client.getMixFormat()
	if err != nil {
		client.release()
		return nil, fmt.Errorf("get mix format: %w", err)
	}
	if err := client.initialize(shareModeShared, streamFlags, hnsMilliseconds(200), mixFormat); err != nil {
		client.release()
		return nil, fmt.Errorf("initialize capture (%s): %w", formatSummary(mixFormat), err)
	}
	svc, err := client.getService(iidIAudioCaptureClient)
	if err != nil {
		client.release()
		return nil, err
	}
	cc := &captureClient{obj: svc}

	conv := newCaptureConverter(mixFormat, opts.Format)

	ringFrames := opts.RingFrames
	if ringFrames <= 0 {
		ringFrames = opts.Format.Rate / 4
	}
	ring := audio.NewRing(ringFrames*opts.Format.BytesPerFrame(), opts.Format.BytesPerFrame())

	logger := opts.Logger
	if logger == nil {
		logger = logging.Nop()
	}
	c := &Capture{
		format: opts.Format,
		ring:   ring,
		logger: logger,
		client: client,
		done:   make(chan struct{}),
		gone:   make(chan struct{}),
	}
	go c.run(cc, conv, devID, devName)
	return c, nil
}

// run is the capture goroutine: poll WASAPI, convert, push to ring.
func (c *Capture) run(cc *captureClient, conv *captureConverter, devID, devName string) {
	runtime.LockOSThread()
	if err := coInitialize(); err != nil {
		c.logger.Errorf("wasapi capture COM init: %v", err)
		close(c.gone)
		return
	}
	defer coUninitialize()
	defer func() {
		cc.release()
		close(c.gone)
	}()

	c.logger.Infof("wasapi capture started: %s (%s)", devName, devID)
	defer c.logger.Infof("wasapi capture stopped: %s", devID)

	if err := c.client.start(); err != nil {
		c.logger.Errorf("wasapi capture start: %v", err)
		return
	}
	defer c.client.stop()

	pcm := make([]byte, 0, 8192)
	for {
		select {
		case <-c.done:
			return
		default:
		}

		n, err := cc.nextPacketSize()
		if err != nil {
			select {
			case <-c.done:
				return
			default:
			}
			c.logger.Errorf("wasapi capture loop: %v", err)
			return
		}
		if n == 0 {
			// Poll with a short sleep; check for shutdown between polls.
			select {
			case <-c.done:
				return
			case <-time.After(capturePollInterval):
			}
			continue
		}

		data, frames, flags, err := cc.getBuffer()
		if err != nil {
			c.logger.Errorf("wasapi get buffer: %v", err)
			return
		}
		if frames > 0 {
			if flags&bufferFlagsSilent != 0 {
				pcm = conv.convertSilence(pcm[:0], int(frames))
			} else {
				pcm = conv.convert(pcm[:0], data, int(frames))
			}
			c.ring.TryWrite(pcm)
		}
		cc.releaseBuffer(frames)
	}
}

func (c *Capture) Format() audio.Format { return c.format }

func (c *Capture) Read(dst []byte) int { return c.ring.Read(dst) }

func (c *Capture) Start() error { return nil } // started by the capture goroutine

func (c *Capture) Close() error {
	c.stopOnce.Do(func() {
		close(c.done)
		<-c.gone // goroutine releases its own COM objects
		if c.client != nil {
			c.client.release()
			c.client = nil
		}
	})
	return nil
}

// captureConverter converts the device mix format to S16LE at the engine
// sample rate, resampling when the rates differ.
type captureConverter struct {
	inBytesPerFrame int
	inIsFloat       bool
	inBits          int
	inChannels      int
	inRate          int
	outChannels     int
	outRate         int
	needsResample   bool
	resampleFrac    float64
}

func newCaptureConverter(mix *waveFormat, out audio.Format) *captureConverter {
	return &captureConverter{
		inBytesPerFrame: mix.Channels * mix.Bits / 8,
		inIsFloat:       mix.IsFloat,
		inBits:          mix.Bits,
		inChannels:      mix.Channels,
		inRate:          mix.SampleRate,
		outChannels:     out.Channels,
		outRate:         out.Rate,
		needsResample:   mix.SampleRate != out.Rate,
	}
}

func (cv *captureConverter) outputFrames(inFrames int) int {
	if !cv.needsResample {
		return inFrames
	}
	return int(float64(inFrames)*float64(cv.outRate)/float64(cv.inRate)) + 2
}

// convert converts `frames` frames at dataPtr into dst (S16LE), appending.
// dst must have capacity for cv.outputFrames(frames) * outChannels * 2 bytes.
func (cv *captureConverter) convert(dst []byte, data unsafe.Pointer, frames int) []byte {
	if frames <= 0 {
		return dst
	}
	if cv.inRate == cv.outRate && cv.inChannels == cv.outChannels && cv.inIsFloat && cv.inBits == 32 {
		return appendF32ToS16(dst, unsafeSlice(data, frames*cv.inChannels*4))
	}

	// Decode input to float64 interleaved at device rate/channels.
	in := decodeToFloat64(data, frames, cv.inChannels, cv.inIsFloat, cv.inBits, cv.inBytesPerFrame)

	// Channel map: first min(in,out) channels, mono-upmix if needed.
	mapped := mapChannels(in, frames, cv.inChannels, cv.outChannels)

	if cv.needsResample {
		outFrames := cv.outputFrames(frames)
		out := make([]float64, 0, outFrames*cv.outChannels)
		out, cv.resampleFrac = resampleLinear(mapped, frames, cv.outChannels, cv.inRate, cv.outRate, cv.resampleFrac, out)
		return appendF64ToS16(dst, out, cv.outChannels)
	}
	return appendF64ToS16(dst, mapped, cv.outChannels)
}

func (cv *captureConverter) convertSilence(dst []byte, frames int) []byte {
	outFrames := frames
	if cv.needsResample {
		outFrames = cv.outputFrames(frames)
	}
	n := outFrames * cv.outChannels * 2
	start := len(dst)
	dst = append(dst, make([]byte, n)...)
	clear(dst[start:])
	return dst
}

func decodeToFloat64(data unsafe.Pointer, frames, channels int, isFloat bool, bits, bytesPerFrame int) []float64 {
	total := frames * channels
	out := make([]float64, total)
	switch {
	case isFloat && bits == 32:
		f := unsafe.Slice((*float32)(data), total)
		for i := 0; i < total; i++ {
			out[i] = float64(f[i])
		}
	case !isFloat && bits == 16:
		s := unsafe.Slice((*int16)(data), total)
		for i := 0; i < total; i++ {
			out[i] = float64(s[i]) / 32768.0
		}
	case !isFloat && bits == 32:
		s := unsafe.Slice((*int32)(data), total)
		for i := 0; i < total; i++ {
			out[i] = float64(s[i]) / 2147483648.0
		}
	default:
		// Unsupported; emit silence.
		clear(out)
	}
	return out
}

func mapChannels(in []float64, frames, inCh, outCh int) []float64 {
	if inCh == outCh {
		return in
	}
	out := make([]float64, frames*outCh)
	n := min(inCh, outCh)
	for f := 0; f < frames; f++ {
		for c := 0; c < n; c++ {
			out[f*outCh+c] = in[f*inCh+c]
		}
		// Mono to stereo upmix duplicates into both channels.
		if inCh == 1 && outCh == 2 {
			out[f*outCh+1] = in[f*inCh]
		}
	}
	return out
}

// resampleLinear resamples interleaved audio with linear interpolation.
// frac carries sub-frame position state between calls.
func resampleLinear(in []float64, inFrames, inCh, inRate, outRate int, frac float64, dst []float64) ([]float64, float64) {
	ratio := float64(inRate) / float64(outRate)
	pos := frac
	for pos < float64(inFrames-1) {
		i := int(pos)
		t := pos - float64(i)
		for c := 0; c < inCh; c++ {
			a := in[i*inCh+c]
			b := in[(i+1)*inCh+c]
			dst = append(dst, a+(b-a)*t)
		}
		pos += ratio
	}
	return dst, pos - float64(inFrames-1)
}

func appendF32ToS16(dst []byte, src []byte) []byte {
	n := len(src) / 4
	for i := 0; i < n; i++ {
		v := float64(math.Float32frombits(binary.LittleEndian.Uint32(src[i*4:])))
		dst = binary.LittleEndian.AppendUint16(dst, f64ToU16(v))
	}
	return dst
}

func appendF64ToS16(dst []byte, src []float64, channels int) []byte {
	for i := 0; i+channels <= len(src); i += channels {
		for c := 0; c < channels; c++ {
			dst = binary.LittleEndian.AppendUint16(dst, f64ToU16(src[i+c]))
		}
	}
	return dst
}

func f64ToU16(v float64) uint16 {
	if v >= 1.0 {
		return 32767
	}
	if v <= -1.0 {
		return 32768
	}
	s := int16(math.Round(v * 32767))
	return uint16(s)
}

func formatSummary(wf *waveFormat) string {
	kind := "pcm"
	if wf.IsFloat {
		kind = "float"
	}
	return fmt.Sprintf("%dHz %dch %d-bit %s", wf.SampleRate, wf.Channels, wf.Bits, kind)
}

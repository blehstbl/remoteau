//go:build opus

package codec

import (
	"fmt"

	"github.com/hraban/opus"

	"remote-au/internal/logging"
)

// opusCodec wraps libopus with FEC, DTX and PLC.
type opusCodec struct {
	cfg    Config
	enc    *opus.Encoder
	dec    *opus.Decoder
	logger logging.Logger

	pcmBuf []float32
	encIn  []float32
	decOut []float32
}

func newOpusCodec(cfg Config, logger logging.Logger) (Codec, error) {
	app, err := opusApp(cfg.AppID)
	if err != nil {
		return nil, err
	}
	enc, err := opus.NewEncoder(cfg.SampleRate, cfg.Channels, app)
	if err != nil {
		return nil, fmt.Errorf("opus encoder: %w", err)
	}
	bitrate := cfg.Bitrate
	if bitrate == 0 {
		bitrate = 96000
	}
	if err := enc.SetBitrate(bitrate); err != nil {
		return nil, fmt.Errorf("opus bitrate: %w", err)
	}
	complexity := cfg.Complexity
	if complexity == 0 {
		complexity = 5
	}
	if err := enc.SetComplexity(complexity); err != nil {
		return nil, fmt.Errorf("opus complexity: %w", err)
	}
	if err := enc.SetInBandFEC(cfg.FEC); err != nil {
		return nil, fmt.Errorf("opus fec: %w", err)
	}
	if err := enc.SetPacketLossPerc(expectedLoss(cfg)); err != nil {
		return nil, fmt.Errorf("opus loss perc: %w", err)
	}
	if err := enc.SetDTX(cfg.DTX); err != nil {
		return nil, fmt.Errorf("opus dtx: %w", err)
	}

	dec, err := opus.NewDecoder(cfg.SampleRate, cfg.Channels)
	if err != nil {
		return nil, fmt.Errorf("opus decoder: %w", err)
	}

	samples := cfg.FrameSamples() * cfg.Channels
	return &opusCodec{
		cfg:    cfg,
		enc:    enc,
		dec:    dec,
		logger: logger,
		pcmBuf: make([]float32, samples),
		encIn:  make([]float32, samples),
		decOut: make([]float32, samples*2),
	}, nil
}

func opusApp(id int) (opus.Application, error) {
	switch id {
	case OpusAppAudio:
		return opus.AppAudio, nil
	case OpusAppVoIP:
		return opus.AppVoIP, nil
	case OpusAppLowDelay:
		return opus.AppRestrictedLowdelay, nil
	default:
		return opus.AppAudio, nil
	}
}

// Opus application ids shared with the wire protocol.
const (
	OpusAppAudio    = 0
	OpusAppVoIP     = 1
	OpusAppLowDelay = 2
)

func expectedLoss(cfg Config) int {
	if cfg.FEC {
		return 10
	}
	return 0
}

func (o *opusCodec) Name() string    { return "opus" }
func (o *opusCodec) Config() Config  { return o.cfg }
func (o *opusCodec) FrameBytes() int { return o.cfg.PCMFrameBytes() }
func (o *opusCodec) WireMTU() int    { return 1024 }

func (o *opusCodec) EncodeFrame(pcm []byte) ([]byte, error) {
	if len(pcm) != o.cfg.PCMFrameBytes() {
		return nil, fmt.Errorf("opus encode: want %d bytes, got %d", o.cfg.PCMFrameBytes(), len(pcm))
	}
	s16ToF32(o.encIn, pcm)
	out := make([]byte, 1024)
	n, err := o.enc.EncodeFloat32(o.encIn, out)
	if err != nil {
		return nil, fmt.Errorf("opus encode: %w", err)
	}
	return out[:n], nil
}

func (o *opusCodec) DecodeFrame(wire []byte, lost bool, out []byte) (int, error) {
	var n int
	var err error
	if lost || len(wire) == 0 {
		n, err = o.dec.DecodePLCFloat32(o.decOut)
	} else {
		n, err = o.dec.DecodeFloat32(wire, o.decOut)
	}
	if err != nil {
		return 0, fmt.Errorf("opus decode: %w", err)
	}
	want := o.cfg.PCMFrameBytes()
	if n*2 > want {
		n = want / 2
	}
	f32ToS16(out[:n*2], o.decOut[:n])
	return n * 2, nil
}

// DecodeFEC implements codec.FECDecoder: reconstructs the PCM of the frame
// *before* wire from the in-band FEC data it carries. hraban/opus's
// DecodeFECFloat32(data, pcm) returns only an error and requires a buffer of
// exactly the missing frame duration; when the packet carries no FEC data
// libopus automatically falls back to PLC, so a nil error can still mean
// synthesized audio (callers treat any error as "fall back to PLC" and the
// output as best-effort recovery either way).
func (o *opusCodec) DecodeFEC(wire []byte, out []byte) (int, error) {
	if len(wire) == 0 {
		return 0, fmt.Errorf("opus fec decode: no data supplied")
	}
	samples := o.cfg.FrameSamples() * o.cfg.Channels
	if err := o.dec.DecodeFECFloat32(wire, o.decOut[:samples]); err != nil {
		return 0, fmt.Errorf("opus fec decode: %w", err)
	}
	n := samples
	want := o.cfg.PCMFrameBytes()
	if n*2 > want {
		n = want / 2
	}
	f32ToS16(out[:n*2], o.decOut[:n])
	return n * 2, nil
}

// SetBitrate updates the encoder bitrate (adaptive quality).
func (o *opusCodec) SetBitrate(bps int) error {
	return o.enc.SetBitrate(bps)
}

// SetFEC updates FEC and expected-loss configuration.
func (o *opusCodec) SetFEC(enabled bool, lossPct int) error {
	if err := o.enc.SetInBandFEC(enabled); err != nil {
		return err
	}
	return o.enc.SetPacketLossPerc(lossPct)
}

func (o *opusCodec) Close() error { return nil }

func s16ToF32(dst []float32, src []byte) {
	n := len(src) / 2
	for i := 0; i < n && i < len(dst); i++ {
		v := int16(uint16(src[i*2]) | uint16(src[i*2+1])<<8)
		dst[i] = float32(v) / 32768.0
	}
}

func f32ToS16(dst []byte, src []float32) {
	for i, v := range src {
		if i*2+1 >= len(dst) {
			break
		}
		s := int16(clampF(float64(v) * 32767.0))
		dst[i*2] = byte(s & 0xFF)
		dst[i*2+1] = byte(uint16(s) >> 8)
	}
}

func clampF(v float64) float64 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return v
}

// opusAvailable is true when built with the opus build tag.
const opusAvailable = true

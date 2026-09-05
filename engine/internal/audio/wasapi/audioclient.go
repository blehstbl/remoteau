//go:build windows

package wasapi

import (
	"encoding/binary"
	"fmt"
	"unsafe"
)

// audioClient wraps IAudioClient.
type audioClient struct {
	obj unsafe.Pointer
}

func (c *audioClient) release() {
	if c.obj != nil {
		callCom(vtable(c.obj)[2], c.obj)
		c.obj = nil
	}
}

// waveFormat is a parsed view of the mix format.
type waveFormat struct {
	SampleRate  int
	Channels    int
	Bits        int
	IsFloat     bool
	Extensible  bool
	ValidBits   int
	ChannelMask uint32

	raw     []byte // owned copy of the WAVEFORMATEX blob
	rawSize uint32
}

const (
	waveFormatPCM       = 0x0001
	waveFormatIEEEFloat = 0x0003
	waveFormatExtensible = 0xFFFE
)

// getMixFormat calls IAudioClient::GetMixFormat and parses the format.
func (c *audioClient) getMixFormat() (*waveFormat, error) {
	var pwfx unsafe.Pointer
	hr := callCom(vtable(c.obj)[8], c.obj, uintptr(unsafe.Pointer(&pwfx)))
	if hr != 0 {
		return nil, hrErr("IAudioClient::GetMixFormat", hr)
	}
	if pwfx == nil {
		return nil, fmt.Errorf("GetMixFormat returned nil")
	}
	defer coTaskMemFree(pwfx)

	cbSize := int(binary.LittleEndian.Uint16(unsafe.Slice((*byte)(pwfx), 18)[16:]))
	total := 18 + cbSize
	raw := make([]byte, total)
	copy(raw, unsafe.Slice((*byte)(pwfx), total))

	tag := binary.LittleEndian.Uint16(raw[0:2])
	channels := int(binary.LittleEndian.Uint16(raw[2:4]))
	rate := int(binary.LittleEndian.Uint32(raw[4:8]))
	bits := int(binary.LittleEndian.Uint16(raw[14:16]))

	wf := &waveFormat{
		SampleRate: rate,
		Channels:   channels,
		Bits:       bits,
		raw:        raw,
		rawSize:    uint32(total),
	}

	switch tag {
	case waveFormatIEEEFloat:
		wf.IsFloat = true
	case waveFormatPCM:
		wf.IsFloat = false
	case waveFormatExtensible:
		wf.Extensible = true
		// WAVEFORMATEXTENSIBLE: after WAVEFORMATEX (18 bytes):
		// wValidBitsPerSample u16, dwChannelMask u32, SubFormat GUID 16 bytes.
		if total < 40 {
			return nil, fmt.Errorf("WAVEFORMATEXTENSIBLE too short: %d", total)
		}
		wf.ValidBits = int(binary.LittleEndian.Uint16(raw[18:20]))
		wf.ChannelMask = binary.LittleEndian.Uint32(raw[20:24])
		sub := (*guid)(unsafe.Pointer(&raw[24]))
		switch {
		case *sub == *subFormatIEEEFloat:
			wf.IsFloat = true
		case *sub == *subFormatPCM:
			wf.IsFloat = false
		default:
			return nil, fmt.Errorf("unsupported SubFormat GUID %v", *sub)
		}
	default:
		return nil, fmt.Errorf("unsupported wave format tag 0x%04x", tag)
	}

	if wf.Channels <= 0 || wf.Channels > 8 || wf.SampleRate < 8000 || wf.SampleRate > 384000 {
		return nil, fmt.Errorf("implausible mix format: %d Hz %d ch", wf.SampleRate, wf.Channels)
	}
	return wf, nil
}

// initialize configures the client. bufferDuration is in 100ns units.
func (c *audioClient) initialize(shareMode, streamFlags int, bufferDuration uint64, mixFormat *waveFormat) error {
	hr := callCom(vtable(c.obj)[3], c.obj,
		uintptr(shareMode), uintptr(streamFlags),
		uintptr(bufferDuration), 0,
		uintptr(unsafe.Pointer(&mixFormat.raw[0])), 0)
	if hr != 0 {
		return hrErr("IAudioClient::Initialize", hr)
	}
	return nil
}

func (c *audioClient) bufferSize() (uint32, error) {
	var n uint32
	hr := callCom(vtable(c.obj)[4], c.obj, uintptr(unsafe.Pointer(&n)))
	if hr != 0 {
		return 0, hrErr("IAudioClient::GetBufferSize", hr)
	}
	return n, nil
}

func (c *audioClient) currentPadding() (uint32, error) {
	var n uint32
	hr := callCom(vtable(c.obj)[6], c.obj, uintptr(unsafe.Pointer(&n)))
	if hr != 0 {
		return 0, hrErr("IAudioClient::GetCurrentPadding", hr)
	}
	return n, nil
}

func (c *audioClient) start() error {
	if hr := callCom(vtable(c.obj)[10], c.obj); hr != 0 {
		return hrErr("IAudioClient::Start", hr)
	}
	return nil
}

func (c *audioClient) stop() {
	_ = callCom(vtable(c.obj)[11], c.obj)
}

func (c *audioClient) setEventHandle(h uintptr) error {
	if hr := callCom(vtable(c.obj)[13], c.obj, h); hr != 0 {
		return hrErr("IAudioClient::SetEventHandle", hr)
	}
	return nil
}

func (c *audioClient) getService(iid *guid) (unsafe.Pointer, error) {
	var out unsafe.Pointer
	hr := callCom(vtable(c.obj)[14], c.obj, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out)))
	if hr != 0 {
		return nil, hrErr("IAudioClient::GetService", hr)
	}
	return out, nil
}

// captureClient wraps IAudioCaptureClient.
type captureClient struct {
	obj unsafe.Pointer
}

func (c *captureClient) release() {
	if c.obj != nil {
		callCom(vtable(c.obj)[2], c.obj)
		c.obj = nil
	}
}

// getBuffer returns (dataPtr, frames, flags, error).
func (c *captureClient) getBuffer() (unsafe.Pointer, uint32, uint32, error) {
	var data unsafe.Pointer
	var frames, flags uint32
	hr := callCom(vtable(c.obj)[3], c.obj,
		uintptr(unsafe.Pointer(&data)),
		uintptr(unsafe.Pointer(&frames)),
		uintptr(unsafe.Pointer(&flags)),
		0, 0)
	if hr != 0 {
		return nil, 0, 0, hrErr("IAudioCaptureClient::GetBuffer", hr)
	}
	return data, frames, flags, nil
}

func (c *captureClient) releaseBuffer(frames uint32) {
	_ = callCom(vtable(c.obj)[4], c.obj, uintptr(frames))
}

func (c *captureClient) nextPacketSize() (uint32, error) {
	var n uint32
	hr := callCom(vtable(c.obj)[5], c.obj, uintptr(unsafe.Pointer(&n)))
	if hr != 0 {
		return 0, hrErr("IAudioCaptureClient::GetNextPacketSize", hr)
	}
	return n, nil
}

// renderClient wraps IAudioRenderClient.
type renderClient struct {
	obj unsafe.Pointer
}

func (c *renderClient) release() {
	if c.obj != nil {
		callCom(vtable(c.obj)[2], c.obj)
		c.obj = nil
	}
}

func (c *renderClient) getBuffer(frames uint32) (unsafe.Pointer, error) {
	var data unsafe.Pointer
	hr := callCom(vtable(c.obj)[3], c.obj, uintptr(frames), uintptr(unsafe.Pointer(&data)))
	if hr != 0 {
		return nil, hrErr("IAudioRenderClient::GetBuffer", hr)
	}
	return data, nil
}

func (c *renderClient) releaseBuffer(frames uint32, flags uint32) {
	_ = callCom(vtable(c.obj)[4], c.obj, uintptr(frames), uintptr(flags))
}

func unsafeBytePtr(p unsafe.Pointer, off int) *byte {
	return (*byte)(unsafe.Pointer(uintptr(p) + uintptr(off)))
}

func unsafeSlice(p unsafe.Pointer, n int) []byte {
	return unsafe.Slice((*byte)(p), n)
}

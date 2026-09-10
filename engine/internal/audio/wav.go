package audio

import (
	"encoding/binary"
	"errors"
	"os"
)

// WAVWriter records S16LE interleaved PCM to a RIFF/WAVE file (Phase 14:
// optional local recording on the sender or receiver).
type WAVWriter struct {
	f        *os.File
	rate     int
	channels int
	dataLen  uint32
}

// NewWAVWriter creates the file and writes the RIFF header (sizes patched on
// Close).
func NewWAVWriter(path string, rate, channels int) (*WAVWriter, error) {
	if rate <= 0 || channels <= 0 {
		return nil, errors.New("wav: bad format")
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	w := &WAVWriter{f: f, rate: rate, channels: channels}

	var hdr [44]byte
	copy(hdr[0:4], "RIFF")
	putU32(hdr[4:], 36) // patched on close
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	putU32(hdr[16:], 16)                      // fmt chunk size
	putU16(hdr[20:], 1)                       // PCM
	putU16(hdr[22:], uint16(channels))        // channels
	putU32(hdr[24:], uint32(rate))            // sample rate
	putU32(hdr[28:], uint32(rate*channels*2)) // byte rate
	putU16(hdr[32:], uint16(channels*2))      // block align
	putU16(hdr[34:], 16)                      // bits
	copy(hdr[36:40], "data")
	putU32(hdr[40:], 0) // patched on close

	if _, err := f.Write(hdr[:]); err != nil {
		_ = f.Close()
		return nil, err
	}
	return w, nil
}

// Write appends S16LE PCM.
func (w *WAVWriter) Write(pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}
	if _, err := w.f.Write(pcm); err != nil {
		return err
	}
	w.dataLen += uint32(len(pcm))
	return nil
}

// Close patches the RIFF sizes and closes the file.
func (w *WAVWriter) Close() error {
	// data chunk size at offset 40; RIFF size at offset 4 = 36 + dataLen.
	var b4 [4]byte
	putU32(b4[:], w.dataLen)
	if _, err := w.f.WriteAt(b4[:], 40); err != nil {
		_ = w.f.Close()
		return err
	}
	putU32(b4[:], 36+w.dataLen)
	if _, err := w.f.WriteAt(b4[:], 4); err != nil {
		_ = w.f.Close()
		return err
	}
	return w.f.Close()
}

func putU32(dst []byte, v uint32) {
	dst[0] = byte(v)
	dst[1] = byte(v >> 8)
	dst[2] = byte(v >> 16)
	dst[3] = byte(v >> 24)
}

func putU16(dst []byte, v uint16) {
	dst[0] = byte(v)
	dst[1] = byte(v >> 8)
}

var _ = binary.LittleEndian

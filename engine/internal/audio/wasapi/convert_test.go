//go:build windows

package wasapi

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestF64ToU16Clamps(t *testing.T) {
	tests := []struct {
		in   float64
		want uint16
	}{
		{in: 0, want: 32767 & 0}, // zero → 0
		{in: 1.0, want: 32767},
		{in: 2.0, want: 32767},
		{in: -1.0, want: 32768},
		{in: -2.0, want: 32768},
		{in: 0.5, want: uint16(int16(math.Round(0.5 * 32767)))},
	}
	for _, tt := range tests {
		if got := f64ToU16(tt.in); got != tt.want {
			t.Fatalf("f64ToU16(%v)=%d, want %d", tt.in, got, tt.want)
		}
	}
	// Zero must be zero.
	if f64ToU16(0) != 0 {
		t.Fatal("f64ToU16(0) != 0")
	}
}

func TestAppendF32ToS16(t *testing.T) {
	src := make([]byte, 8)
	binary.LittleEndian.PutUint32(src[0:], math.Float32bits(1.0))
	binary.LittleEndian.PutUint32(src[4:], math.Float32bits(-0.5))
	out := appendF32ToS16(nil, src)
	if len(out) != 4 {
		t.Fatalf("len=%d, want 4", len(out))
	}
	if got := int16(binary.LittleEndian.Uint16(out[0:])); got != 32767 {
		t.Fatalf("first sample=%d, want 32767", got)
	}
	if got := int16(binary.LittleEndian.Uint16(out[2:])); math.Abs(float64(got)+0.5*32767) > 2 {
		t.Fatalf("second sample=%d, want ≈ -16383", got)
	}
}

func TestS16ResamplerIdentityRate(t *testing.T) {
	// 48k→48k must be created only when rates differ; here we still test the
	// math by using a 2x ratio and known samples.
	r := newS16Resampler(48000, 1, 96000)
	pull := func(out []byte, frames uint32) int {
		// A 1 kHz ramp: sample i has value (i%100)*100 as int16.
		n := 0
		for i := 0; i < int(frames) && n < int(frames)*2; i++ {
			v := int16((i % 100) * 100)
			binary.LittleEndian.PutUint16(out[n:], uint16(v))
			n += 2
		}
		return n
	}
	out := make([]byte, 100*2)
	frames := r.pull(pull, out, 100)
	if frames != 100 {
		t.Fatalf("frames=%d, want 100", frames)
	}
	// Values must be within the input ramp range (no wraparound garbage).
	for i := 0; i < 100; i++ {
		v := int16(binary.LittleEndian.Uint16(out[i*2:]))
		if v < 0 || v >= 10000 {
			t.Fatalf("sample %d = %d out of range", i, v)
		}
	}
}

func TestMapChannelsMonoUpmix(t *testing.T) {
	in := []float64{0.5, -0.5}
	out := mapChannels(in, 2, 1, 2)
	want := []float64{0.5, 0.5, -0.5, -0.5}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("out[%d]=%v, want %v", i, out[i], want[i])
		}
	}
}

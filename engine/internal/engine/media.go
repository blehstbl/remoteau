package engine

import (
	"time"

	"remote-au/internal/codec"
)

// clientMedia is the v2 receiver's media pipeline: reorder window + PLC +
// network measurement, feeding decoded PCM to the consumer. Runs on the
// media-loop goroutine only (no locks needed beyond the config swap).
type clientMedia struct {
	codec codec.Codec
	out   func(pcm []byte)

	// Reorder window.
	nextSeq    uint32
	seen       bool
	future     map[uint32][]byte
	holdPeriod time.Duration
	lastFlush  time.Time

	// Stats (EWMA where noted).
	packetsSeen      uint64
	lossPackets      uint64
	latePackets      uint64
	reorderedPackets uint64
	concealedFrames  uint64
	lossEWMA         float64
	lateEWMA         float64
	jitterMsEWMA     float64
	bufMsEWMA        float64
	lastArrival      time.Time

	// Burst-loss tracking.
	burstLoss      int
	maxBurstLoss   int
	totalLossFrames uint64

	frameMs float64
}

func newClientMedia(c codec.Codec, out func(pcm []byte)) *clientMedia {
	frameMs := 10.0
	if c != nil {
		// Recover frame duration from the codec config's frame size.
		if cfg := c.Config(); cfg.FrameMs > 0 {
			frameMs = float64(cfg.FrameMs)
		}
	}
	hold := time.Duration(frameMs*1.5) * time.Millisecond
	if hold < 2*time.Millisecond {
		hold = 2 * time.Millisecond
	}
	return &clientMedia{
		codec:      c,
		out:        out,
		future:     make(map[uint32][]byte),
		holdPeriod: hold,
		frameMs:    frameMs,
	}
}

// accept feeds one received wire frame.
func (m *clientMedia) accept(seq uint32, flags uint8, payload []byte) {
	now := time.Now()
	m.packetsSeen++

	// Inter-arrival jitter.
	if !m.lastArrival.IsZero() && m.frameMs > 0 {
		dev := now.Sub(m.lastArrival).Seconds()*1000.0 - m.frameMs
		if dev < 0 {
			dev = -dev
		}
		m.jitterMsEWMA += 0.1 * (dev - m.jitterMsEWMA)
	}
	m.lastArrival = now

	// Copy payload: the receive buffer is reused.
	buf := make([]byte, len(payload))
	copy(buf, payload)

	if !m.seen {
		m.seen = true
		m.nextSeq = seq + 1
		m.emit(buf, false)
		m.lastFlush = now
		return
	}

	delta := int32(seq - m.nextSeq)
	switch {
	case delta < 0:
		// Stale or duplicate.
		m.latePackets++
		m.lateEWMA += 0.05 * (1 - m.lateEWMA)
		return
	case delta == 0:
		m.emit(buf, false)
		m.nextSeq = seq + 1
		m.flush(now, false)
		return
	default:
		// Future packet: hold it and let the flush path conceal the gap.
		m.reorderedPackets++
		if len(m.future) < 64 {
			m.future[seq] = buf
		}
		m.flush(now, false)
		return
	}
}

// tick flushes held frames after the reorder hold expires.
func (m *clientMedia) tick() {
	if m.seen {
		m.flush(time.Now(), true)
	}
}

// flush writes consecutive held frames and conceals gaps. When forced
// (hold expired) missing frames are concealed; otherwise only consecutive
// frames are emitted.
func (m *clientMedia) flush(now time.Time, force bool) {
	if !m.seen {
		return
	}
	if !force && now.Sub(m.lastFlush) < m.holdPeriod {
		return
	}
	m.lastFlush = now

	for {
		if buf, ok := m.future[m.nextSeq]; ok {
			delete(m.future, m.nextSeq)
			m.nextSeq++
			m.emit(buf, false)
			continue
		}
		// Missing the next frame.
		if len(m.future) == 0 {
			break
		}
		if !force {
			// Not forcing: wait for the hold period before concealing.
			break
		}
		// Conceal one frame and advance.
		m.nextSeq++
		m.emit(nil, true)
	}

	m.bufMsEWMA += 0.2 * (float64(len(m.future))*m.frameMs - m.bufMsEWMA)
}

// emitSilence delivers one frame of silence (DTX frames arrive as explicit
// silence markers, not loss).
func (m *clientMedia) emitSilence() {
	if m.out == nil || m.codec == nil {
		return
	}
	out := make([]byte, m.codec.FrameBytes())
	clear(out)
	m.out(out)
}

// emit decodes one wire frame (or conceals when payload is nil) and delivers
// PCM to the consumer.
func (m *clientMedia) emit(wire []byte, lost bool) {
	if m.out == nil || m.codec == nil {
		return
	}
	if lost {
		m.lossPackets++
		m.totalLossFrames++
		m.lossEWMA += 0.05 * (1 - m.lossEWMA)
		m.burstLoss++
		if m.burstLoss > m.maxBurstLoss {
			m.maxBurstLoss = m.burstLoss
		}
	} else {
		m.burstLoss = 0
	}
	out := make([]byte, m.codec.FrameBytes())
	n, err := m.codec.DecodeFrame(wire, lost, out)
	if err != nil {
		// Decode failure: treat as loss for stats but don't spam the sink.
		return
	}
	if lost {
		m.concealedFrames += uint64(n / 4) // approx stereo frames
	}
	m.out(out[:n])
}

// snapshot returns the current stats for reporting.
func (m *clientMedia) snapshot() (loss, late, jitterMs, bufMs float64, seen, lossPk, latePk, reorderPk, concealed uint64, maxBurst int) {
	return m.lossEWMA, m.lateEWMA, m.jitterMsEWMA, m.bufMsEWMA,
		m.packetsSeen, m.lossPackets, m.latePackets, m.reorderedPackets,
		m.concealedFrames, m.maxBurstLoss
}

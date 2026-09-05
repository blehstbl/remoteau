package audio

import (
	"sync"
	"sync/atomic"
)

// Ring is a bounded, mutex-guarded PCM ring buffer intended for hand-off
// between an audio-device callback and a consumer goroutine. Writers may use
// TryWrite to avoid ever blocking the callback; overflow drops oldest bytes.
type Ring struct {
	mu      sync.Mutex
	buf     []byte
	quantum int
	read    int
	used    int
	dropped atomic.Uint64
}

func NewRing(sizeBytes, quantum int) *Ring {
	if quantum <= 0 {
		quantum = 1
	}
	if sizeBytes < quantum {
		sizeBytes = quantum
	}
	sizeBytes -= sizeBytes % quantum
	if sizeBytes == 0 {
		sizeBytes = quantum
	}

	return &Ring{
		buf:     make([]byte, sizeBytes),
		quantum: quantum,
	}
}

func (r *Ring) TryWrite(p []byte) int {
	if len(p) == 0 {
		return 0
	}
	if !r.mu.TryLock() {
		r.dropped.Add(uint64(r.alignedLen(len(p))))
		return 0
	}
	defer r.mu.Unlock()
	return r.writeLocked(p)
}

func (r *Ring) Read(dst []byte) int {
	if len(dst) == 0 {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readLocked(dst)
}

func (r *Ring) TryRead(dst []byte) int {
	if len(dst) == 0 {
		return 0
	}
	if !r.mu.TryLock() {
		return 0
	}
	defer r.mu.Unlock()
	return r.readLocked(dst)
}

func (r *Ring) Available() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.used
}

func (r *Ring) Dropped() uint64 {
	return r.dropped.Load()
}

func (r *Ring) writeLocked(p []byte) int {
	n := r.alignedLen(len(p))
	if n == 0 {
		r.dropped.Add(uint64(len(p)))
		return 0
	}
	if n != len(p) {
		r.dropped.Add(uint64(len(p) - n))
		p = p[:n]
	}

	if len(p) > len(r.buf) {
		drop := len(p) - len(r.buf)
		r.dropped.Add(uint64(drop))
		p = p[drop:]
	}

	space := len(r.buf) - r.used
	if len(p) > space {
		drop := alignUp(len(p)-space, r.quantum)
		if drop > r.used {
			drop = r.used
		}
		r.read = (r.read + drop) % len(r.buf)
		r.used -= drop
		r.dropped.Add(uint64(drop))
	}

	write := (r.read + r.used) % len(r.buf)
	copied := copy(r.buf[write:], p)
	if copied < len(p) {
		copy(r.buf, p[copied:])
	}
	r.used += len(p)
	return len(p)
}

func (r *Ring) readLocked(dst []byte) int {
	n := r.alignedLen(len(dst))
	if n > r.used {
		n = r.used
	}
	n = r.alignedLen(n)
	if n == 0 {
		return 0
	}

	first := min(n, len(r.buf)-r.read)
	copied := copy(dst, r.buf[r.read:r.read+first])
	if copied < n {
		copy(dst[copied:], r.buf[:n-copied])
	}
	r.read = (r.read + n) % len(r.buf)
	r.used -= n
	return n
}

func (r *Ring) alignedLen(n int) int {
	return n - n%r.quantum
}

func alignUp(n, quantum int) int {
	if n <= 0 {
		return 0
	}
	rem := n % quantum
	if rem == 0 {
		return n
	}
	return n + quantum - rem
}

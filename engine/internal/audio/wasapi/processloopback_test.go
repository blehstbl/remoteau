//go:build windows

package wasapi

import (
	"strings"
	"testing"
)

// Pure-validation tests for process loopback (no COM touched; live capture
// cannot be validated on this machine — it has zero audio endpoints).
func TestValidateProcessLoopbackTargets(t *testing.T) {
	pids, err := validateProcessLoopbackTargets(nil, false)
	if err == nil || !strings.Contains(err.Error(), "no target process ids") {
		t.Fatalf("empty pids + process-target: want clear error, got %v (%v)", pids, err)
	}

	pids, err = validateProcessLoopbackTargets(nil, true)
	if err == nil {
		t.Fatalf("empty pids + exclude: want error")
	}

	pids, err = validateProcessLoopbackTargets([]uint32{0}, false)
	if err == nil || !strings.Contains(err.Error(), "pid 0") {
		t.Fatalf("pid 0: want error, got %v", err)
	}

	// Exclude mode takes exactly one PID (one activation per API call).
	pids, err = validateProcessLoopbackTargets([]uint32{11, 22}, true)
	if err == nil || !strings.Contains(err.Error(), "single pid") {
		t.Fatalf("exclude with 2 pids: want error, got %v", err)
	}

	// Duplicates are collapsed; order preserved.
	pids, err = validateProcessLoopbackTargets([]uint32{7, 7, 3}, false)
	if err != nil || len(pids) != 2 || pids[0] != 7 || pids[1] != 3 {
		t.Fatalf("dedupe: got %v err %v", pids, err)
	}

	if pids, err = validateProcessLoopbackTargets([]uint32{42}, true); err != nil || len(pids) != 1 || pids[0] != 42 {
		t.Fatalf("single exclude: got %v err %v", pids, err)
	}
}

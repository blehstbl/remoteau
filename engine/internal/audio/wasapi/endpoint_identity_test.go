//go:build windows

package wasapi

import "testing"

// Regression test for the endpoint property-store reader: when Windows has an
// active default render endpoint, its id and friendly name must be non-empty.
// Skips on machines with no active endpoint (e.g. audio disabled).
func TestDefaultRenderEndpointIdentity(t *testing.T) {
	if err := coInitialize(); err != nil {
		t.Fatalf("coinit: %v", err)
	}
	defer coUninitialize()

	enum, err := newMMDeviceEnumerator()
	if err != nil {
		t.Fatalf("enum: %v", err)
	}
	defer enum.release()

	dev, err := enum.getDefaultAudioEndpoint(dataFlowRender, roleConsole)
	if err != nil {
		t.Skipf("no default render endpoint on this machine: %v", err)
	}
	defer dev.release()

	id, err := dev.id()
	if err != nil || id == "" {
		t.Fatalf("device id: %q err=%v", id, err)
	}
	name := dev.friendlyName()
	if name == "" {
		t.Fatal("friendly name is empty (property-store read regressed)")
	}
	t.Logf("default render: %q id=%s", name, id)
}

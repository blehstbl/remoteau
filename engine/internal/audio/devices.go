package audio

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// SelectDeviceBySelector resolves a device selector (index or
// case-insensitive name substring) against a device list.
func SelectDeviceBySelector(selector string, devices []DeviceInfo, kind string) (DeviceInfo, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return DeviceInfo{}, fmt.Errorf("%s device selector is empty", kind)
	}

	if index, err := strconv.Atoi(selector); err == nil {
		if index < 0 {
			return DeviceInfo{}, fmt.Errorf("%s device index must be non-negative: %d", kind, index)
		}
		for _, device := range devices {
			if device.Index == index {
				return device, nil
			}
		}
		return DeviceInfo{}, fmt.Errorf("%s device index %d out of range; available %s devices: %s", kind, index, kind, formatDeviceCandidates(devices))
	}

	if match, ok, err := selectNamedDevice(selector, devices, true); ok || err != nil {
		return match, err
	}
	if match, ok, err := selectNamedDevice(selector, devices, false); ok || err != nil {
		return match, err
	}

	return DeviceInfo{}, fmt.Errorf("no %s device matching %q; available %s devices: %s", kind, selector, kind, formatDeviceCandidates(devices))
}

func selectNamedDevice(selector string, devices []DeviceInfo, exact bool) (DeviceInfo, bool, error) {
	lowerSelector := strings.ToLower(selector)
	matches := make([]DeviceInfo, 0, 1)
	for _, device := range devices {
		name := strings.ToLower(device.Name)
		if (exact && name == lowerSelector) || (!exact && strings.Contains(name, lowerSelector)) {
			matches = append(matches, device)
		}
	}
	if len(matches) == 0 {
		return DeviceInfo{}, false, nil
	}
	if len(matches) == 1 {
		return matches[0], true, nil
	}
	return DeviceInfo{}, true, fmt.Errorf("device selector %q is ambiguous; matching devices: %s", selector, formatDeviceCandidates(matches))
}

func formatDeviceCandidates(devices []DeviceInfo) string {
	if len(devices) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(devices))
	for _, device := range devices {
		parts = append(parts, fmt.Sprintf("[%d] %s", device.Index, device.Name))
	}
	return strings.Join(parts, "; ")
}

// PrintDevices writes a human-readable device listing.
func PrintDevices(w io.Writer, lists DeviceLists) error {
	fmt.Fprintln(w, "Playback devices:")
	printDeviceList(w, lists.Playback)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Capture devices:")
	printDeviceList(w, lists.Capture)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Loopback: %s\n", lists.LoopbackNote)
	return nil
}

// EncodeDeviceListsJSON writes the device lists as indented JSON.
func EncodeDeviceListsJSON(w io.Writer, lists DeviceLists) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(lists)
}

func printDeviceList(w io.Writer, devices []DeviceInfo) {
	if len(devices) == 0 {
		fmt.Fprintln(w, "  (none)")
		return
	}
	for _, device := range devices {
		if device.Note == "" {
			fmt.Fprintf(w, "  [%d] %s\n", device.Index, device.Name)
			continue
		}
		fmt.Fprintf(w, "  [%d] %s - %s\n", device.Index, device.Name, device.Note)
	}
}

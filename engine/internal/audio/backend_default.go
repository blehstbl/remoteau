package audio

// defaultBackendName picks the default backend for this platform/build.
// Windows uses the pure-Go WASAPI backend; other platforms use miniaudio when
// the binary was built with the malgo build tag.
func defaultBackendName(goos string) string {
	if goos == "windows" {
		return "wasapi"
	}
	if _, ok := backendRegistry["malgo"]; ok {
		return "malgo"
	}
	return "none"
}

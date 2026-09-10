//go:build windows

package wasapi

import (
	"unicode/utf16"
	"unsafe"
)

// mmDeviceEnumerator wraps IMMDeviceEnumerator.
type mmDeviceEnumerator struct {
	obj unsafe.Pointer
}

func newMMDeviceEnumerator() (*mmDeviceEnumerator, error) {
	obj, err := coCreateInstance(clsidMMDeviceEnumerator, iidIMMDeviceEnumerator)
	if err != nil {
		return nil, err
	}
	return &mmDeviceEnumerator{obj: obj}, nil
}

func (e *mmDeviceEnumerator) release() {
	if e.obj != nil {
		callCom(vtable(e.obj)[2], e.obj)
		e.obj = nil
	}
}

// getDefaultAudioEndpoint returns the IMMDevice for the default endpoint.
func (e *mmDeviceEnumerator) getDefaultAudioEndpoint(dataFlow, role int) (*mmDevice, error) {
	var out unsafe.Pointer
	hr := callCom(vtable(e.obj)[4], e.obj, uintptr(dataFlow), uintptr(role), uintptr(unsafe.Pointer(&out)))
	if hr != 0 {
		return nil, hrErr("GetDefaultAudioEndpoint", hr)
	}
	return &mmDevice{obj: out}, nil
}

// enumAudioEndpoints returns active endpoints for a data flow.
func (e *mmDeviceEnumerator) enumAudioEndpoints(dataFlow int) ([]*mmDevice, error) {
	var coll unsafe.Pointer
	hr := callCom(vtable(e.obj)[3], e.obj, uintptr(dataFlow), uintptr(stateActive), uintptr(unsafe.Pointer(&coll)))
	if hr != 0 {
		return nil, hrErr("EnumAudioEndpoints", hr)
	}
	defer callCom(vtable(coll)[2], coll) // Release

	var count uint32
	hr = callCom(vtable(coll)[3], coll, uintptr(unsafe.Pointer(&count)))
	if hr != 0 {
		return nil, hrErr("IMMDeviceCollection::GetCount", hr)
	}

	devices := make([]*mmDevice, 0, count)
	for i := uint32(0); i < count; i++ {
		var dev unsafe.Pointer
		hr = callCom(vtable(coll)[4], coll, uintptr(i), uintptr(unsafe.Pointer(&dev)))
		if hr != 0 {
			continue
		}
		devices = append(devices, &mmDevice{obj: dev})
	}
	return devices, nil
}

// mmDevice wraps IMMDevice.
type mmDevice struct {
	obj unsafe.Pointer
}

func (d *mmDevice) release() {
	if d.obj != nil {
		callCom(vtable(d.obj)[2], d.obj)
		d.obj = nil
	}
}

// activate returns the requested interface pointer.
func (d *mmDevice) activate(iid *guid) (unsafe.Pointer, error) {
	var out unsafe.Pointer
	hr := callCom(vtable(d.obj)[3], d.obj,
		uintptr(unsafe.Pointer(iid)), uintptr(clsCtxAll), 0, uintptr(unsafe.Pointer(&out)))
	if hr != 0 {
		return nil, hrErr("IMMDevice::Activate", hr)
	}
	return out, nil
}

// id returns the endpoint id string.
func (d *mmDevice) id() (string, error) {
	var pwsz unsafe.Pointer
	hr := callCom(vtable(d.obj)[5], d.obj, uintptr(unsafe.Pointer(&pwsz)))
	if hr != 0 {
		return "", hrErr("IMMDevice::GetId", hr)
	}
	defer coTaskMemFree(pwsz)
	return utf16PtrToString(pwsz), nil
}

// friendlyName reads the friendly name from the endpoint property store.
func (d *mmDevice) friendlyName() string {
	var store unsafe.Pointer
	hr := callCom(vtable(d.obj)[4], d.obj, uintptr(stgmRead), uintptr(unsafe.Pointer(&store)))
	if hr != 0 {
		return ""
	}
	defer callCom(vtable(store)[2], store)

	// PROPERTYKEY {a45c254e-df1c-4efd-8020-67d146a850e0}, pid 14
	key := struct {
		fmtid guid
		pid   uint32
	}{fmtid: *newGUID("{A45C254E-DF1C-4EFD-8020-67D146A850E0}"), pid: 14}

	// PROPVARIANT: 32 bytes is enough on both x86 and x64.
	var pv [32]byte
	hr = callCom(vtable(store)[5], store, uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&pv[0])))
	if hr != 0 {
		return ""
	}
	// Free the PROPVARIANT only AFTER reading it (IPropertyStore has no
	// PropVariantClear; ole32 does).
	defer clearPropVariant(&pv)

	vt := *(*uint16)(unsafe.Pointer(&pv[0]))
	if vt != 31 { // VT_LPWSTR
		return ""
	}
	ptr := *(*unsafe.Pointer)(unsafe.Pointer(&pv[8]))
	if ptr == nil {
		return ""
	}
	return utf16PtrToString(ptr)
}

var procPropVariantClear = ole32.NewProc("PropVariantClear")

func clearPropVariant(pv *[32]byte) {
	procPropVariantClear.Call(uintptr(unsafe.Pointer(pv)))
}

func utf16PtrToString(p unsafe.Pointer) string {
	if p == nil {
		return ""
	}
	wide := unsafe.Slice((*uint16)(p), 4096)
	n := 0
	for n < len(wide) && wide[n] != 0 {
		n++
	}
	return string(utf16.Decode(wide[:n]))
}

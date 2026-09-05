package main

import (
	"fmt"
	"os"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32         = windows.NewLazySystemDLL("user32.dll")
	procMessageBox = user32.NewProc("MessageBoxW")
)

func messageBox(title, text string) {
	titlePtr, _ := windows.UTF16PtrFromString(title)
	textPtr, _ := windows.UTF16PtrFromString(text)
	procMessageBox.Call(0, uintptr(unsafe.Pointer(textPtr)), uintptr(unsafe.Pointer(titlePtr)), 0)
}

func executableDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	for i := len(exe) - 1; i >= 0; i-- {
		if exe[i] == '\\' || exe[i] == '/' {
			return exe[:i]
		}
	}
	return "."
}

func osCommand(name, arg string) *exec.Cmd {
	return exec.Command(name, arg)
}

// trayIconICO synthesizes a valid 16x16 32bpp ICO at runtime (a rounded
// green badge) so the tray has a native icon without external assets.
func trayIconICO() []byte {
	const size = 16
	// BITMAPINFOHEADER (40 bytes) for a 16x16 32bpp image.
	bih := []byte{
		40, 0, 0, 0, // header size
		size, 0, 0, 0, // width
		size * 2, 0, 0, 0, // height (XOR + AND)
		1, 0,       // planes
		32, 0,      // bpp
		0, 0, 0, 0, // compression BI_RGB
		0, 0, 0, 0, // image size (0 for BI_RGB)
		0, 0, 0, 0, // x ppm
		0, 0, 0, 0, // y ppm
		0, 0, 0, 0, // colors used
		0, 0, 0, 0, // important colors
	}

	// Pixel data (BGRA), bottom-up: a green rounded square with a dark core.
	pix := make([]byte, size*size*4)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			// Rounded corners.
			corner := false
			for _, c := range [][2]int{{2, 2}, {13, 2}, {2, 13}, {13, 13}} {
				dx := x - c[0]
				dy := y - c[1]
				if (x < 3 || x > 12) && (y < 3 || y > 12) && dx*dx+dy*dy > 2 {
					corner = true
				}
			}
			off := ((size - 1 - y)*size + x) * 4
			if corner {
				// transparent
				continue
			}
			inner := x > 4 && x < 11 && y > 4 && y < 11
			if inner {
				pix[off+0] = 0x1E // B
				pix[off+1] = 0x88 // G
				pix[off+2] = 0x2A // R
			} else {
				pix[off+0] = 0x55 // B
				pix[off+1] = 0xC2 // G
				pix[off+2] = 0x40 // R
			}
			pix[off+3] = 0xFF // A
		}
	}

	// AND mask (1bpp, all zeros = use alpha).
	and := make([]byte, size*4) // 16 rows * 16 bits / 8 = 32B; padded to 4B per row
	_ = and

	img := append(bih, pix...)
	img = append(img, and...)

	// ICO header + one directory entry.
	ico := make([]byte, 0, 6+16+len(img))
	ico = append(ico, 0, 0, 1, 0, 1, 0) // ICONDIR: reserved, type=1 icon, count=1
	entry := []byte{
		size, size, // width, height
		0,    // color count
		0,    // reserved
		1, 0, // planes
		32, 0, // bpp
		byte(len(img)), byte(len(img) >> 8), byte(len(img) >> 16), byte(len(img) >> 24), // size
		22, 0, 0, 0, // offset (6 + 16)
	}
	ico = append(ico, entry...)
	ico = append(ico, img...)
	return ico
}

var _ = fmt.Sprintf

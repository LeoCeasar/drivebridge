//go:build windows

package platform

import (
	"context"
	"errors"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var linkUser32 = windows.NewLazySystemDLL("user32.dll")
var linkKernel32 = windows.NewLazySystemDLL("kernel32.dll")

type windowsLinkClipboard struct{ window uintptr }

func NewLinkClipboardWriter(ctx context.Context) (func(string) error, error) {
	return newLinkClipboardWriter(ctx, &windowsLinkClipboard{})
}

func (*windowsLinkClipboard) sequence() (uint32, error) {
	seq, _, _ := linkUser32.NewProc("GetClipboardSequenceNumber").Call()
	if seq == 0 {
		return 0, errors.New("clipboard sequence unavailable")
	}
	return uint32(seq), nil
}

func (c *windowsLinkClipboard) withLock(ctx context.Context, fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// A real owner is required by EmptyClipboard/SetClipboardData. HWND_MESSAGE
	// keeps this window invisible and out of the taskbar and activation order.
	class, _ := windows.UTF16PtrFromString("STATIC")
	hwnd, _, err := linkUser32.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(class)), 0, 0, 0, 0, 0, 0, ^uintptr(2), 0, 0, 0)
	if hwnd == 0 {
		return err
	}
	defer linkUser32.NewProc("DestroyWindow").Call(hwnd)
	c.window = hwnd
	defer func() { c.window = 0 }()
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ok, _, _ := linkUser32.NewProc("OpenClipboard").Call(hwnd)
		if ok != 0 {
			break
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer linkUser32.NewProc("CloseClipboard").Call()
	return fn()
}

// write is called only with the clipboard exclusively open on this OS thread.
func (*windowsLinkClipboard) write(ctx context.Context, text string) error {
	data, err := windows.UTF16FromString(text)
	if err != nil {
		return err
	}
	h, _, err := linkKernel32.NewProc("GlobalAlloc").Call(0x0002, uintptr(len(data)*2)) // GMEM_MOVEABLE
	if h == 0 {
		return err
	}
	defer func() {
		if h != 0 {
			linkKernel32.NewProc("GlobalFree").Call(h)
		}
	}()
	ptr, _, err := linkKernel32.NewProc("GlobalLock").Call(h)
	if ptr == 0 {
		return err
	}
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(ptr)), len(data)), data)
	if ok, _, err := linkKernel32.NewProc("GlobalUnlock").Call(h); ok == 0 && err != windows.ERROR_SUCCESS {
		return err
	}
	// Allocate and fill before emptying, so allocation failure preserves content.
	if err := ctx.Err(); err != nil {
		return err
	}
	if ok, _, err := linkUser32.NewProc("EmptyClipboard").Call(); ok == 0 {
		return err
	}
	if ok, _, err := linkUser32.NewProc("SetClipboardData").Call(13, h); ok == 0 {
		return err
	} // CF_UNICODETEXT
	h = 0 // ownership transferred to Windows
	return nil
}

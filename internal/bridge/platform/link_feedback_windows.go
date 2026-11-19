//go:build windows

package platform

import (
	"context"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// LinkFeedback is silent on success. Failure UI never activates or waits for a
// user response; Explorer is free to keep processing file operations.
func LinkFeedback(ctx context.Context, message string, failed bool) {
	linkFeedback(ctx, message, failed, showLinkErrorWindow)
}

type linkWindowRect struct{ left, top, right, bottom int32 }
type linkWindowMessage struct {
	window         uintptr
	message        uint32
	wParam, lParam uintptr
	time           uint32
	x, y           int32
	private        uint32
}

// Caller owns the OS thread and destroys the returned HWND on that same thread.
func createLinkErrorWindow(message string) (uintptr, error) {
	class, _ := windows.UTF16PtrFromString("STATIC")
	title, _ := windows.UTF16PtrFromString("DriveBridge · 飞书分享链接")
	text, err := windows.UTF16PtrFromString("DriveBridge · 飞书分享链接\r\n\r\n" + message)
	if err != nil {
		return 0, err
	}
	var work linkWindowRect
	if ok, _, err := linkUser32.NewProc("SystemParametersInfoW").Call(0x0030, 0, uintptr(unsafe.Pointer(&work)), 0); ok == 0 {
		return 0, err
	} // SPI_GETWORKAREA
	width := min(int32(420), work.right-work.left)
	height := min(int32(144), work.bottom-work.top)
	x, y := work.right-width-16, work.bottom-height-16
	if x < work.left {
		x = work.left
	}
	if y < work.top {
		y = work.top
	}
	// WS_EX_NOACTIVATE | WS_EX_TOOLWINDOW | WS_EX_TOPMOST;
	// WS_POPUP | WS_BORDER | SS_ETCHEDFRAME (host never paints its title).
	hwnd, _, err := linkUser32.NewProc("CreateWindowExW").Call(0x08000088, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)), 0x80800012, uintptr(x), uintptr(y), uintptr(width), uintptr(height), 0, 0, 0, 0)
	if hwnd == 0 {
		return 0, err
	}
	label, _, err := linkUser32.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(text)), 0x50000000, 16, 16, uintptr(width-32), uintptr(height-32), hwnd, 0, 0, 0)
	if label == 0 {
		linkUser32.NewProc("DestroyWindow").Call(hwnd)
		return 0, err
	}
	font, _, _ := windows.NewLazySystemDLL("gdi32.dll").NewProc("GetStockObject").Call(17) // DEFAULT_GUI_FONT
	linkUser32.NewProc("SendMessageW").Call(label, 0x0030, font, 1)                        // WM_SETFONT
	// SetWindowPos ignores the launcher STARTUPINFO show state, including SW_HIDE.
	if ok, _, err := linkUser32.NewProc("SetWindowPos").Call(hwnd, ^uintptr(0), 0, 0, 0, 0, 0x0053); ok == 0 { // TOPMOST; SHOWWINDOW|NOACTIVATE|NOMOVE|NOSIZE
		linkUser32.NewProc("DestroyWindow").Call(hwnd)
		return 0, err
	}
	return hwnd, nil
}

func showLinkErrorWindow(ctx context.Context, message string) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hwnd, err := createLinkErrorWindow(message)
	if err != nil {
		return
	} // No modal/external-program fallback.
	defer linkUser32.NewProc("DestroyWindow").Call(hwnd)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var msg linkWindowMessage
	for {
		if ctx.Err() != nil {
			return
		}
		for {
			if ctx.Err() != nil {
				return
			}
			available, _, _ := linkUser32.NewProc("PeekMessageW").Call(uintptr(unsafe.Pointer(&msg)), hwnd, 0, 0, 1)
			if available == 0 {
				break
			}
			linkUser32.NewProc("TranslateMessage").Call(uintptr(unsafe.Pointer(&msg)))
			linkUser32.NewProc("DispatchMessageW").Call(uintptr(unsafe.Pointer(&msg)))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

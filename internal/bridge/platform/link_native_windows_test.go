//go:build windows

package platform

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func requireWindowsLinkNative(t *testing.T) {
	t.Helper()
	if os.Getenv("DRIVEBRIDGE_TEST_LINK_NATIVE") != "1" {
		t.Skip("isolated Windows clipboard/desktop required")
	}
}

// Independent Unicode reader: GlobalUnlock may return zero on successful last
// unlock of a movable block. Do not use atotto/clipboard's affected reader here.
func readWindowsLinkClipboard(t *testing.T) string {
	t.Helper()
	var text string
	err := (&windowsLinkClipboard{}).withLock(context.Background(), func() error {
		h, _, err := linkUser32.NewProc("GetClipboardData").Call(13)
		if h == 0 {
			return err
		}
		size, _, err := linkKernel32.NewProc("GlobalSize").Call(h)
		if size < 2 || size > 1<<20 {
			return errors.New("unexpected clipboard size")
		}
		ptr, _, err := linkKernel32.NewProc("GlobalLock").Call(h)
		if ptr == 0 {
			return err
		}
		text = windows.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Pointer(ptr)), int(size/2)))
		if ok, _, err := linkKernel32.NewProc("GlobalUnlock").Call(h); ok == 0 && err != windows.ERROR_SUCCESS {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func TestWindowsLinkClipboardUnicodeAndChanged(t *testing.T) {
	requireWindowsLinkNative(t)
	c := &windowsLinkClipboard{}
	if err := c.withLock(context.Background(), func() error { return c.write(context.Background(), "原始剪贴板") }); err != nil {
		t.Fatal(err)
	}
	write, err := NewLinkClipboardWriter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := write("https://tenant.feishu.cn/docx/中文"); err != nil {
		t.Fatal(err)
	}
	if text := readWindowsLinkClipboard(t); text != "https://tenant.feishu.cn/docx/中文" {
		t.Fatal(text)
	}
	write, err = NewLinkClipboardWriter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.withLock(context.Background(), func() error { return c.write(context.Background(), "用户新内容") }); err != nil {
		t.Fatal(err)
	}
	if err := write("stale result"); !errors.Is(err, ErrLinkClipboardChanged) {
		t.Fatal(err)
	}
	if text := readWindowsLinkClipboard(t); text != "用户新内容" {
		t.Fatal(text)
	}
}

func TestWindowsLinkClipboardBusyPreservesContent(t *testing.T) {
	requireWindowsLinkNative(t)
	c := &windowsLinkClipboard{}
	if err := c.withLock(context.Background(), func() error { return c.write(context.Background(), "keep while busy") }); err != nil {
		t.Fatal(err)
	}
	write, err := NewLinkClipboardWriter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = c.withLock(context.Background(), func() error {
		done := make(chan error, 1)
		go func() { done <- write("must not replace") }()
		select {
		case err := <-done:
			return err
		case <-time.After(2 * time.Second):
			t.Fatal("clipboard wait unbounded")
			return nil
		}
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if text := readWindowsLinkClipboard(t); text != "keep while busy" {
		t.Fatal(text)
	}
}

func TestWindowsLinkClipboardCancelledWritePreservesContent(t *testing.T) {
	requireWindowsLinkNative(t)
	c := &windowsLinkClipboard{}
	if err := c.withLock(context.Background(), func() error { return c.write(context.Background(), "keep on cancel") }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	err := c.withLock(context.Background(), func() error {
		cancel()
		return c.write(ctx, "cancelled late result")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if text := readWindowsLinkClipboard(t); text != "keep on cancel" {
		t.Fatal(text)
	}
}

func TestWindowsLinkErrorWindowHiddenLaunch(t *testing.T) {
	requireWindowsLinkNative(t)
	if os.Getenv("DRIVEBRIDGE_LINK_HIDDEN_CHILD") == "1" {
		TestWindowsLinkErrorWindowDoesNotActivate(t)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWindowsLinkErrorWindowHiddenLaunch$", "-test.count=1")
	cmd.Env = append(os.Environ(), "DRIVEBRIDGE_LINK_HIDDEN_CHILD=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hidden-launch error panel: %v\n%s", err, output)
	}
}

func TestWindowsLinkErrorWindowDoesNotActivate(t *testing.T) {
	requireWindowsLinkNative(t)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	foreground, _, _ := linkUser32.NewProc("GetForegroundWindow").Call()
	hwnd, err := createLinkErrorWindow("测试：剪贴板已变化，请重试。")
	if err != nil {
		t.Fatal(err)
	}
	defer linkUser32.NewProc("DestroyWindow").Call(hwnd)
	style, _, _ := linkUser32.NewProc("GetWindowLongW").Call(hwnd, ^uintptr(19)) // GWL_EXSTYLE = -20
	if style&0x08000080 != 0x08000080 {
		t.Fatalf("missing NOACTIVATE/TOOLWINDOW: %#x", style)
	}
	visible, _, _ := linkUser32.NewProc("IsWindowVisible").Call(hwnd)
	after, _, _ := linkUser32.NewProc("GetForegroundWindow").Call()
	if visible == 0 || after != foreground {
		t.Fatalf("visible=%d foreground changed=%v", visible, after != foreground)
	}
}

func TestWindowsLinkFeedbackSilentAndBounded(t *testing.T) {
	requireWindowsLinkNative(t)
	started := time.Now()
	LinkFeedback(context.Background(), "success", false)
	if time.Since(started) > 250*time.Millisecond {
		t.Fatal("successful copy waited for feedback")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	started = time.Now()
	LinkFeedback(ctx, "测试错误自动消失", true)
	if elapsed := time.Since(started); elapsed < 60*time.Millisecond || elapsed > time.Second {
		t.Fatal(elapsed)
	}
	class, _ := windows.UTF16PtrFromString("STATIC")
	title, _ := windows.UTF16PtrFromString("DriveBridge · 飞书分享链接")
	hwnd, _, _ := linkUser32.NewProc("FindWindowW").Call(uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)))
	if hwnd != 0 {
		t.Fatal("error window leaked after timeout")
	}
}

func TestWindowsLinkFeedbackDeadlineUnderMessages(t *testing.T) {
	requireWindowsLinkNative(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { LinkFeedback(ctx, "测试消息队列繁忙时自动关闭", true); close(done) }()
	class, _ := windows.UTF16PtrFromString("STATIC")
	title, _ := windows.UTF16PtrFromString("DriveBridge · 飞书分享链接")
	var hwnd uintptr
	for hwnd == 0 && ctx.Err() == nil {
		hwnd, _, _ = linkUser32.NewProc("FindWindowW").Call(uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)))
		if hwnd == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	if hwnd == 0 {
		t.Fatal("error window was not created")
	}
	stop := make(chan struct{})
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			post := linkUser32.NewProc("PostMessageW")
			for {
				select {
				case <-stop:
					return
				default:
					post.Call(hwnd, 0, 0, 0)
				}
			}
		}()
	}
	timedOut := false
	select {
	case <-done:
	case <-time.After(time.Second):
		timedOut = true
	}
	close(stop)
	workers.Wait()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("feedback loop leaked")
	}
	if timedOut {
		t.Fatal("queued messages prevented deadline dismissal")
	}
}

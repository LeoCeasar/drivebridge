package larkcli

import (
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CLI requests use pipes instead of a terminal, including when launched from the
// tray's GUI process. Keep the child attached for normal cancellation/reaping.
func configureCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

// Start suspended so no descendant can escape between process creation and job
// assignment. Unsupported job admission fails closed before CLI code executes.
func runBusinessCommand(cmd *exec.Cmd) (bool, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return false, &Error{Type: "process", Subtype: "containment"}
	}
	defer windows.CloseHandle(job)
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return false, &Error{Type: "process", Subtype: "containment"}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	cmd.WaitDelay = 250 * time.Millisecond
	var canceled atomic.Bool
	cmd.Cancel = func() error {
		canceled.Store(true)
		return windows.TerminateJobObject(job, 1)
	}
	if err = cmd.Start(); err != nil {
		return false, &Error{Type: "process", Subtype: "start"}
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err == nil {
		err = windows.AssignProcessToJobObject(job, process)
		windows.CloseHandle(process)
	}
	if err == nil {
		if canceled.Load() {
			err = windows.ERROR_OPERATION_ABORTED
		} else {
			err = resumeCLIThread(uint32(cmd.Process.Pid))
		}
	}
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return true, &Error{Type: "process", Subtype: "containment"}
	}
	err = cmd.Wait()
	// Also terminate helpers after an otherwise successful parent exit.
	_ = windows.TerminateJobObject(job, 1)
	return true, err
}

func resumeCLIThread(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if openErr != nil {
			return openErr
		}
		_, resumeErr := windows.ResumeThread(thread)
		windows.CloseHandle(thread)
		return resumeErr
	}
	return err
}

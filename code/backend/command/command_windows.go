//go:build windows

package command

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const processStartsSuspended = true

type processTree struct {
	job      windows.Handle
	attached bool
}

type processWaitResult struct {
	observed bool
	err      error
	reaped   bool
}

var assignWindowsProcessToJob = windows.AssignProcessToJobObject
var resumeWindowsProcess = resumeSuspendedWindowsProcess

func (tree *processTree) active(_ *exec.Cmd) bool { return tree.attached }

func configureWindowsCommand(process *exec.Cmd) {
	process.SysProcAttr = &syscall.SysProcAttr{
		HideWindow: true,
		CreationFlags: windows.CREATE_NO_WINDOW |
			windows.CREATE_NEW_PROCESS_GROUP |
			windows.CREATE_SUSPENDED,
	}
}

func newProcessTree() (*processTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	return &processTree{job: job}, nil
}

func (tree *processTree) attach(process *exec.Cmd) error {
	if process.Process == nil {
		return fmt.Errorf("engine process is missing")
	}
	var assignErr error
	if err := process.Process.WithHandle(func(handle uintptr) {
		assignErr = assignWindowsProcessToJob(tree.job, windows.Handle(handle))
	}); err != nil {
		return err
	}
	if assignErr != nil {
		return assignErr
	}
	tree.attached = true
	if err := resumeWindowsProcess(uint32(process.Process.Pid)); err != nil {
		return fmt.Errorf("resume assigned engine process: %w", err)
	}
	return nil
}

func resumeSuspendedWindowsProcess(processID uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return err
	}
	for {
		if entry.OwnerProcessID == processID {
			thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if openErr != nil {
				return openErr
			}
			previous, resumeErr := windows.ResumeThread(thread)
			closeErr := windows.CloseHandle(thread)
			if resumeErr != nil {
				return resumeErr
			}
			if previous == 0 {
				return fmt.Errorf("engine process primary thread was not suspended")
			}
			return closeErr
		}
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			return fmt.Errorf("find engine process primary thread: %w", err)
		}
	}
}

func (tree *processTree) wait(process *exec.Cmd) processWaitResult {
	return processWaitResult{err: process.Wait(), reaped: true}
}

func (tree *processTree) reap(_ *exec.Cmd) error { return nil }

func (tree *processTree) terminate(process *exec.Cmd) error {
	if tree.job != 0 && tree.attached {
		if err := windows.TerminateJobObject(tree.job, 1); err == nil {
			return nil
		}
	}
	if process.Process == nil {
		return nil
	}
	return process.Process.Kill()
}

func (tree *processTree) close() {
	if tree.job != 0 {
		_ = windows.CloseHandle(tree.job)
		tree.job = 0
	}
}

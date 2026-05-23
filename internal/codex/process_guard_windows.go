//go:build windows

package codex

import (
	"fmt"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

type appServerProcessGuard struct {
	job windows.Handle
}

func guardAppServerProcess(cmd *exec.Cmd) (appServerProcessGuard, error) {
	if cmd == nil || cmd.Process == nil {
		return appServerProcessGuard{}, nil
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return appServerProcessGuard{}, fmt.Errorf("create Codex app-server cleanup job: %w", err)
	}
	guard := appServerProcessGuard{job: job}

	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		uint32(unsafe.Sizeof(limits)),
	); err != nil {
		_ = guard.Close()
		return appServerProcessGuard{}, fmt.Errorf("configure Codex app-server cleanup job: %w", err)
	}

	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		_ = guard.Close()
		return appServerProcessGuard{}, fmt.Errorf("open Codex app-server process for cleanup job: %w", err)
	}
	defer func() {
		_ = windows.CloseHandle(process)
	}()
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		_ = guard.Close()
		return appServerProcessGuard{}, fmt.Errorf("attach Codex app-server to cleanup job: %w", err)
	}
	return guard, nil
}

func (g appServerProcessGuard) Close() error {
	if g.job == 0 {
		return nil
	}
	return windows.CloseHandle(g.job)
}

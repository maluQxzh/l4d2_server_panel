//go:build windows

package agent

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

const (
	createSuspended                   = 0x00000004
	createNewConsole                  = 0x00000010
	createNewProcessGroup             = 0x00000200
	processTerminate                  = 0x0001
	processSetQuota                   = 0x0100
	processSuspendResume              = 0x0800
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x00002000
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW   = kernel32.NewProc("CreateJobObjectW")
	procAssignProcessToJob = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject = kernel32.NewProc("TerminateJobObject")
	procOpenProcess        = kernel32.NewProc("OpenProcess")
	procCloseHandle        = kernel32.NewProc("CloseHandle")
	procSetInformationJob  = kernel32.NewProc("SetInformationJobObject")
	ntdll                  = syscall.NewLazyDLL("ntdll.dll")
	procNtResumeProcess    = ntdll.NewProc("NtResumeProcess")
)

type platformProcess struct{ job syscall.Handle }

func platformProcessOutputFailure(line string) string {
	if strings.Contains(line, "CTextConsoleWin32::GetLine: !GetNumberOfConsoleInputEvents") {
		return "Windows SRCDS console input initialization failed; deploy and restart an Agent build with hidden console-input support"
	}
	return ""
}

type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobObjectIOCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectExtendedLimit struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IOInfo                jobObjectIOCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

func preparePlatformCommand(cmd *exec.Cmd) {
	// Starting suspended closes the gap in which srcds (or one of its native
	// extensions) could create a child before the process belongs to our Job.
	//
	// SRCDS' Win32 text console calls GetNumberOfConsoleInputEvents even when
	// the Agent never sends interactive input. os/exec normally replaces a nil
	// stdin with NUL, which is not a console input-buffer handle and makes SRCDS
	// display an Engine Error dialog. Give each managed server a hidden console
	// and pass a NULL standard-input handle; Windows then initializes stdin from
	// that new console while preserving our redirected stdout and stderr handles.
	// See the STARTF_USESTDHANDLES rules documented for CREATE_NEW_CONSOLE.
	if cmd.Stdin == nil {
		cmd.Stdin = os.NewFile(0, "automatic-console-input")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewConsole | createNewProcessGroup | createSuspended, HideWindow: true}
}

func attachPlatformProcess(cmd *exec.Cmd) (attached *platformProcess, resultErr error) {
	var job, process uintptr
	// Every error path leaves the child suspended. Kill it here rather than
	// relying solely on the caller, and close any partially-created handles.
	// Once assigned, terminating/closing the Job also catches any descendants.
	defer func() {
		if process != 0 {
			procCloseHandle.Call(process)
		}
		if resultErr != nil {
			if job != 0 {
				procTerminateJobObject.Call(job, 1)
				procCloseHandle.Call(job)
			}
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
	}()

	job, _, createErr := procCreateJobObjectW.Call(0, 0)
	if job == 0 {
		return nil, fmt.Errorf("CreateJobObjectW: %w", createErr)
	}
	limits := jobObjectExtendedLimit{}
	limits.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	configured, _, configErr := procSetInformationJob.Call(job, jobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), unsafe.Sizeof(limits))
	if configured == 0 {
		return nil, fmt.Errorf("SetInformationJobObject: %w", configErr)
	}
	process, _, openErr := procOpenProcess.Call(processTerminate|processSetQuota|processSuspendResume, 0, uintptr(uint32(cmd.Process.Pid)))
	if process == 0 {
		return nil, fmt.Errorf("OpenProcess: %w", openErr)
	}
	assigned, _, assignErr := procAssignProcessToJob.Call(job, process)
	if assigned == 0 {
		return nil, fmt.Errorf("AssignProcessToJobObject: %w", assignErr)
	}
	if err := procNtResumeProcess.Find(); err != nil {
		return nil, fmt.Errorf("find NtResumeProcess: %w", err)
	}
	status, _, _ := procNtResumeProcess.Call(process)
	if status != 0 {
		return nil, fmt.Errorf("NtResumeProcess: NTSTATUS 0x%08X", uint32(status))
	}
	attached = &platformProcess{job: syscall.Handle(job)}
	job = 0 // ownership transferred to attached
	return attached, nil
}

// Source servers are first stopped with RCON quit. Windows has no reliable
// service-session equivalent of Ctrl-Break, so the process tree is left alone
// during the grace period and then terminated as one Job Object.
func gracefulPlatformProcess(_ *platformProcess, _ *os.Process) error { return nil }

func forcePlatformProcess(p *platformProcess, process *os.Process) error {
	if p != nil && p.job != 0 {
		ok, _, callErr := procTerminateJobObject.Call(uintptr(p.job), 1)
		if ok != 0 {
			return nil
		}
		if callErr != syscall.Errno(0) {
			return callErr
		}
	}
	return process.Kill()
}

func closePlatformProcess(p *platformProcess) {
	if p != nil && p.job != 0 {
		procCloseHandle.Call(uintptr(p.job))
		p.job = 0
	}
}

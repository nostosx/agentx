package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"
)

// shellCommand returns a command running script with "cmd /d /s /c". The
// command line is passed verbatim: cmd.exe does not parse the escaping
// that exec applies to arguments, so quotes in script would be mangled.
// preferBash is ignored; bash on a Windows PATH is often WSL's, which runs
// in a different environment.
func shellCommand(ctx context.Context, script string, _ bool) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd")
	// /s with the outer quotes makes cmd.exe strip only those quotes and
	// parse the rest as if typed at its prompt: %VAR% is expanded and ^, &,
	// |, < and > keep their meaning. /d skips AutoRun commands from the
	// registry.
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd /d /s /c "` + script + `"`}
	return cmd
}

// killProcessTree makes context cancellation kill cmd and its descendants,
// so children started by the shell do not outlive the command. Commands
// started with Tree.Start use a Job Object instead.
func killProcessTree(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return taskkill(cmd) }
}

// taskkill kills cmd and the descendants taskkill /T can find by walking
// parent process IDs; descendants whose parent already exited are missed.
// Once cmd was waited for it kills nothing and returns os.ErrProcessDone,
// since its process ID may then name another process.
func taskkill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return errors.New("exec: not started")
	}
	// The process handle held during f keeps the ID from being reused.
	err := cmd.Process.WithHandle(func(uintptr) {
		// Kill below covers a failed taskkill.
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run() //nolint:noctx // bounded by the process exiting
	})
	if errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return cmd.Process.Kill()
}

const (
	createSuspended = 0x00000004

	processTerminate     = 0x0001
	processSetQuota      = 0x0100
	processSuspendResume = 0x0800

	jobObjectBasicAccountingInformation = 1
	jobObjectExtendedLimitInformation   = 9
	jobObjectLimitKillOnJobClose        = 0x2000
)

var (
	kernel32                      = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW          = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject   = kernel32.NewProc("SetInformationJobObject")
	procQueryInformationJobObject = kernel32.NewProc("QueryInformationJobObject")
	procAssignProcessToJobObject  = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject        = kernel32.NewProc("TerminateJobObject")
	procNtResumeProcess           = syscall.NewLazyDLL("ntdll.dll").NewProc("NtResumeProcess")
)

// jobBasicLimits is JOBOBJECT_BASIC_LIMIT_INFORMATION.
type jobBasicLimits struct {
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

// jobExtendedLimits is JOBOBJECT_EXTENDED_LIMIT_INFORMATION.
type jobExtendedLimits struct {
	Basic jobBasicLimits
	// C aligns the basic limits to 8 bytes, Go only to 4 on 32-bit
	// systems.
	_                     jobBasicLimitsPad
	IoInfo                [6]uint64 // IO_COUNTERS
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// jobAccounting is JOBOBJECT_BASIC_ACCOUNTING_INFORMATION.
type jobAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// The layouts must match the Windows headers, or SetInformationJobObject
// fails and the tree silently falls back to taskkill.
var (
	_ [unsafe.Offsetof(jobExtendedLimits{}.IoInfo) - jobBasicLimitsSize]struct{}
	_ [jobBasicLimitsSize - unsafe.Offsetof(jobExtendedLimits{}.IoInfo)]struct{}
	_ [unsafe.Sizeof(jobExtendedLimits{}) - jobExtendedLimitsSize]struct{}
	_ [jobExtendedLimitsSize - unsafe.Sizeof(jobExtendedLimits{})]struct{}
	_ [unsafe.Sizeof(jobAccounting{}) - 48]struct{}
	_ [48 - unsafe.Sizeof(jobAccounting{})]struct{}
)

// treeJob is the Job Object of a Tree.
type treeJob struct {
	// h is the job handle, or 0 if the job could not be set up.
	h syscall.Handle
}

// prepareTree makes cmd start suspended, so it can join the job before it
// starts any process of its own.
func prepareTree(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createSuspended
}

func (t *Tree) start() error {
	if err := t.cmd.Start(); err != nil {
		return err
	}
	pid := uint32(t.cmd.Process.Pid)
	h, err := syscall.OpenProcess(processTerminate|processSetQuota|processSuspendResume, false, pid)
	if err != nil {
		_ = t.cmd.Process.Kill()
		_ = t.cmd.Wait()
		return fmt.Errorf("open process %d: %w", pid, err)
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	if job, err := newKillJob(h); err == nil {
		t.mu.Lock()
		t.job.h = job
		t.mu.Unlock()
	}
	if status, _, _ := procNtResumeProcess.Call(uintptr(h)); status != 0 {
		_ = t.Close()
		_ = t.cmd.Process.Kill()
		_ = t.cmd.Wait()
		return fmt.Errorf("resume process %d: NTSTATUS %#x", pid, status)
	}
	return nil
}

// newKillJob creates a Job Object that kills its processes when its last
// handle is closed, and assigns the process h to it.
func newKillJob(h syscall.Handle) (syscall.Handle, error) {
	r, _, err := procCreateJobObjectW.Call(0, 0)
	if r == 0 {
		return 0, err
	}
	job := syscall.Handle(r)
	var info jobExtendedLimits
	info.Basic.LimitFlags = jobObjectLimitKillOnJobClose
	if r, _, err := procSetInformationJobObject.Call(uintptr(job), jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info)); r == 0 {
		_ = syscall.CloseHandle(job)
		return 0, err
	}
	if r, _, err := procAssignProcessToJobObject.Call(uintptr(job), uintptr(h)); r == 0 {
		_ = syscall.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

// activeProcesses returns the number of running processes in job.
func activeProcesses(job syscall.Handle) (uint32, bool) {
	var info jobAccounting
	r, _, _ := procQueryInformationJobObject.Call(uintptr(job), jobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info), 0)
	return info.ActiveProcesses, r != 0
}

// kill terminates the job. The job handle always names this tree's job,
// whereas taskkill names the command by its process ID, so it is used only
// while the command was not waited for.
func (t *Tree) kill(waited bool) error {
	if t.job.h != 0 {
		if n, ok := activeProcesses(t.job.h); ok && n == 0 {
			return os.ErrProcessDone
		}
		if r, _, _ := procTerminateJobObject.Call(uintptr(t.job.h), 1); r != 0 {
			return nil
		}
	}
	if waited {
		return os.ErrProcessDone
	}
	return taskkill(t.cmd)
}

func (t *Tree) release() {
	if t.job.h != 0 {
		_ = syscall.CloseHandle(t.job.h)
		t.job.h = 0
	}
}

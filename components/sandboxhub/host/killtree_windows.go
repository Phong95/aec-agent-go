//go:build windows

package host

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// ---------------------------------------------------------------------------
// Process trees on Windows (CONTEXT B1): a command is started in a Job Object that
// ends everything in it when the job is terminated or its last handle is closed.
//
// "taskkill /pid <pid> /T /F" alone would walk the parent links of the process
// tree: a grandchild whose parent already exited is no longer linked, so it would
// survive. A job holds every descendant whatever its parent did (a child made
// with CreateProcess joins the job of its parent, and neither breakaway flag is
// set), so the orphan dies too. The process is created SUSPENDED, put in the job and
// only then resumed, so it cannot start a grandchild outside the job.
//
// The API is reached through syscall.NewLazyDLL: no module outside the standard
// library is needed (golang.org/x/sys would be the usual way).
// ---------------------------------------------------------------------------

const (
	// jobObjectLimitKillOnJobClose is JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE from
	// JOBOBJECT_BASIC_LIMIT_INFORMATION.LimitFlags (winnt.h, learn.microsoft.com
	// "JOBOBJECT_BASIC_LIMIT_INFORMATION"): the job's processes are terminated when
	// the last handle to the job is closed.
	jobObjectLimitKillOnJobClose = 0x00002000
	// jobObjectExtendedLimitInformation is the JOBOBJECTINFOCLASS value
	// JobObjectExtendedLimitInformation (9) for SetInformationJobObject.
	jobObjectExtendedLimitInformation = 9

	// Process creation flags (winbase.h).
	createSuspended = 0x00000004
	createNoWindow  = 0x08000000

	// Process access rights (winnt.h) NtResumeProcess and AssignProcessToJobObject need.
	processTerminate      = 0x0001
	processSetQuota       = 0x0100
	processSuspendResume  = 0x0800
	assignAndResumeAccess = processSetQuota | processTerminate | processSuspendResume

	// taskkillTimeout bounds the fallback taskkill run.
	taskkillTimeout = 10 * time.Second
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	ntdll            = syscall.NewLazyDLL("ntdll.dll")
	pCreateJobObject = kernel32.NewProc("CreateJobObjectW")
	pSetInfoJob      = kernel32.NewProc("SetInformationJobObject")
	pAssignToJob     = kernel32.NewProc("AssignProcessToJobObject")
	pTerminateJob    = kernel32.NewProc("TerminateJobObject")
	pNtResumeProcess = ntdll.NewProc("NtResumeProcess")
)

// ioCounters is IO_COUNTERS: six ULONGLONGs.
type ioCounters struct {
	ReadOperationCount, WriteOperationCount, OtherOperationCount uint64
	ReadTransferCount, WriteTransferCount, OtherTransferCount    uint64
}

// basicLimitInformation is JOBOBJECT_BASIC_LIMIT_INFORMATION. The field types follow
// the Windows ones (LARGE_INTEGER is int64, SIZE_T and ULONG_PTR are uintptr, DWORD
// is uint32), so the size is right on the 64-bit architectures (64 bytes, 144 with
// the extended part; killtree_windows_test.go checks amd64). On 32-bit Windows Go
// aligns an int64 to 4 bytes where Windows uses 8, the size differs, and
// SetInformationJobObject fails: newJob then reports the error and the command falls
// back to taskkill, as when a process cannot be assigned to a job.
type basicLimitInformation struct {
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

// extendedLimitInformation is JOBOBJECT_EXTENDED_LIMIT_INFORMATION.
type extendedLimitInformation struct {
	BasicLimitInformation basicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// tree is the handle on everything one command started: a Job Object, or - when the
// command could not be put in one - just its process id for the taskkill fallback.
// kill and release are safe to call from several goroutines and in any order; after
// release nothing is touched.
type tree struct {
	pid int

	mu     sync.Mutex
	job    syscall.Handle // 0 when the command is not in a job
	closed bool
}

// newJob creates a Job Object that kills its processes when the last handle to it
// is closed.
func newJob() (syscall.Handle, error) {
	h, _, err := pCreateJobObject.Call(0, 0)
	if h == 0 {
		return 0, err
	}
	var info extendedLimitInformation
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	r, _, err := pSetInfoJob.Call(h, jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info)) //nolint:gosec // G103/G115: the documented calling convention of SetInformationJobObject
	if r == 0 {
		_ = syscall.CloseHandle(syscall.Handle(h))
		return 0, err
	}
	return syscall.Handle(h), nil
}

// startInTree starts cmd in a Job Object and returns the tree. The process is
// created suspended, assigned to the job, and ALWAYS resumed: when it cannot be
// assigned (no job could be made, or the parent sits in a job that forbids
// nesting), it is resumed all the same and the tree falls back to
// "taskkill /pid <pid> /T /F", because a suspended process that nothing
// resumes would hang the command forever (threat T-07-21). A process that cannot be
// opened or resumed at all is killed and the start fails.
//
// cmd.SysProcAttr keeps a CmdLine the shell builder set; the flags are added to
// any CreationFlags already there.
func startInTree(cmd *exec.Cmd) (*tree, error) {
	attr := cmd.SysProcAttr
	if attr == nil {
		attr = &syscall.SysProcAttr{}
	}
	attr.HideWindow = true
	attr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP | createSuspended | createNoWindow
	cmd.SysProcAttr = attr

	job, jobErr := newJob()
	if err := cmd.Start(); err != nil {
		if jobErr == nil {
			_ = syscall.CloseHandle(job)
		}
		return nil, err
	}
	t := &tree{pid: cmd.Process.Pid}

	ph, err := syscall.OpenProcess(assignAndResumeAccess, false, uint32(cmd.Process.Pid)) //nolint:gosec // G115: a process id fits a uint32
	if err != nil {
		abandon(cmd, job, jobErr)
		return nil, err
	}
	defer func() { _ = syscall.CloseHandle(ph) }()

	assigned := jobErr == nil && assignToJob(job, ph)
	// NtResumeProcess returns an NTSTATUS: zero is success.
	if status, _, _ := pNtResumeProcess.Call(uintptr(ph)); status != 0 {
		abandon(cmd, job, jobErr)
		return nil, ntStatusError(status)
	}
	if assigned {
		t.job = job
	} else if jobErr == nil {
		_ = syscall.CloseHandle(job) // an empty job nothing will ever use
	}
	return t, nil
}

// ntStatusError is the error of a failed NtResumeProcess. An NTSTATUS is not a Win32
// error code, so it must not be turned into a syscall.Errno (whose message would be the
// text of an unrelated Win32 error): it is reported as the hexadecimal status.
func ntStatusError(status uintptr) error {
	return fmt.Errorf("NtResumeProcess failed: NTSTATUS 0x%08x", uint32(status)) //nolint:gosec // G115: an NTSTATUS is 32 bits
}

// taskkillProgram is the taskkill to run: System32\taskkill.exe below %SystemRoot% when
// SystemRoot is set, so the fallback does not depend on the server's PATH, and the bare
// name (resolved through PATH) when it is not.
func taskkillProgram() string {
	if root := os.Getenv("SystemRoot"); root != "" {
		return filepath.Join(root, "System32", "taskkill.exe")
	}
	return "taskkill"
}

// assignToJob puts a process in a job and reports whether that worked. It is a
// variable so a test can make the assignment fail and prove the fallback path.
var assignToJob = func(job, process syscall.Handle) bool {
	r, _, _ := pAssignToJob.Call(uintptr(job), uintptr(process))
	return r != 0
}

// abandon kills a process that was started suspended and cannot be resumed, waits
// for it, and closes the job.
func abandon(cmd *exec.Cmd, job syscall.Handle, jobErr error) {
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if jobErr == nil {
		_ = syscall.CloseHandle(job)
	}
}

// kill ends the command and everything it started: TerminateJobObject for a job, the
// taskkill fallback for a command that is not in one (or whose job refuses). It does nothing after release.
func (t *tree) kill() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	if t.job != 0 {
		if r, _, _ := pTerminateJob.Call(uintptr(t.job), 1); r != 0 {
			return nil
		}
	}
	return t.taskkill()
}

// taskkill is the fallback for a command that is in no job: it walks the process
// tree by parent links only, so a grandchild whose parent exited
// first survives it. A command only lands here when AssignProcessToJobObject failed.
func (t *tree) taskkill() error {
	ctx, cancel := context.WithTimeout(context.Background(), taskkillTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, taskkillProgram(), "/pid", strconv.Itoa(t.pid), "/T", "/F") //nolint:gosec // G204: a fixed program and the pid of our own child
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run()
}

// release closes the job handle once the command is over. KILL_ON_JOB_CLOSE ends
// anything a command left running in the job, so a command that started a
// background process does not outlive its Exec call.
func (t *tree) release() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	if t.job != 0 {
		_ = syscall.CloseHandle(t.job)
		t.job = 0
	}
}

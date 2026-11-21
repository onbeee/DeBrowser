package mihomo

import (
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobChild is the Windows childProcess. The core process is
// created suspended, assigned to a Job Object configured with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, and only then resumed — so
// it never runs outside the job, and when DeBrowser's process
// ends for any reason (normal exit, crash, force kill) the
// operating system closes the job handle and terminates the
// core. The job contains exactly this one process: no external
// process (the user's Clash, another Mihomo) is ever added.
type jobChild struct {
	proc windows.Handle
	job  windows.Handle
}

func (c *jobChild) Wait() error {
	if _, err := windows.WaitForSingleObject(c.proc, windows.INFINITE); err != nil {
		return err
	}
	var code uint32
	err := windows.GetExitCodeProcess(c.proc, &code)
	windows.CloseHandle(c.proc)
	if c.job != 0 {
		// The process is already gone; closing the job now
		// cannot kill anything else (it holds no other process).
		windows.CloseHandle(c.job)
	}
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("exit code %d", code)
	}
	return nil
}

func (c *jobChild) Kill() error {
	return windows.TerminateProcess(c.proc, 1)
}

// quoteArg quotes one argument for a Windows command line,
// following the standard CommandLineToArgvW rules.
func quoteArg(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	backslashes := 0
	for _, r := range s {
		switch r {
		case '\\':
			backslashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, backslashes*2+1))
			b.WriteByte('"')
			backslashes = 0
		default:
			b.WriteString(strings.Repeat(`\`, backslashes))
			backslashes = 0
			b.WriteRune(r)
		}
	}
	b.WriteString(strings.Repeat(`\`, backslashes*2))
	b.WriteByte('"')
	return b.String()
}

func init() {
	startCoreChild = func(bin string, args []string, dir string, logFile *os.File) (childProcess, error) {
		job, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			return nil, fmt.Errorf("create job object: %w", err)
		}
		fail := func(err error) (childProcess, error) {
			windows.CloseHandle(job)
			return nil, err
		}
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(job,
			windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
			return fail(fmt.Errorf("configure job object: %w", err))
		}

		// The child's std handles must be inheritable.
		logHandle := windows.Handle(logFile.Fd())
		if err := windows.SetHandleInformation(logHandle,
			windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			return fail(fmt.Errorf("prepare core log handle: %w", err))
		}
		nulName, _ := windows.UTF16PtrFromString("NUL")
		nul, err := windows.CreateFile(nulName, windows.GENERIC_READ,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
			windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			return fail(fmt.Errorf("open NUL for core stdin: %w", err))
		}
		_ = windows.SetHandleInformation(nul,
			windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT)

		parts := make([]string, 0, len(args)+1)
		parts = append(parts, quoteArg(bin))
		for _, a := range args {
			parts = append(parts, quoteArg(a))
		}
		cmdline, err := windows.UTF16PtrFromString(strings.Join(parts, " "))
		if err != nil {
			windows.CloseHandle(nul)
			return fail(err)
		}
		exePtr, err := windows.UTF16PtrFromString(bin)
		if err != nil {
			windows.CloseHandle(nul)
			return fail(err)
		}
		dirPtr, err := windows.UTF16PtrFromString(dir)
		if err != nil {
			windows.CloseHandle(nul)
			return fail(err)
		}
		si := &windows.StartupInfo{
			Flags:     windows.STARTF_USESTDHANDLES,
			StdInput:  nul,
			StdOutput: logHandle,
			StdErr:    logHandle,
		}
		pi := &windows.ProcessInformation{}
		// CREATE_SUSPENDED: the process does not run until it is
		// inside the job and ResumeThread is called.
		err = windows.CreateProcess(exePtr, cmdline, nil, nil, true,
			windows.CREATE_SUSPENDED|windows.CREATE_NO_WINDOW, nil, dirPtr, si, pi)
		windows.CloseHandle(nul)
		if err != nil {
			return fail(fmt.Errorf("create core process: %w", err))
		}
		abort := func(err error) (childProcess, error) {
			windows.TerminateProcess(pi.Process, 1)
			windows.CloseHandle(pi.Process)
			windows.CloseHandle(pi.Thread)
			return fail(err)
		}
		if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
			return abort(fmt.Errorf("assign core to job object: %w", err))
		}
		if _, err := windows.ResumeThread(pi.Thread); err != nil {
			return abort(fmt.Errorf("resume core process: %w", err))
		}
		windows.CloseHandle(pi.Thread)
		return &jobChild{proc: pi.Process, job: job}, nil
	}
}

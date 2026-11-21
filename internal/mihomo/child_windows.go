package mihomo

import (
	"errors"
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

		// The child receives inheritable DUPLICATES of the
		// handles it needs as its standard handles. The
		// originals (the log file handle, the NUL handle) are
		// never modified — in particular their inherit flag is
		// not set — so starting the core leaves no side effects
		// on this process's own handles. The duplicates are
		// closed again as soon as the process is created (the
		// child holds its own copies by then).
		self, err := windows.GetCurrentProcess()
		if err != nil {
			return fail(fmt.Errorf("prepare core handles: %w", err))
		}
		dup := func(h windows.Handle) (windows.Handle, error) {
			var out windows.Handle
			if err := windows.DuplicateHandle(self, h, self, &out, 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
				return 0, err
			}
			return out, nil
		}
		nulName, _ := windows.UTF16PtrFromString("NUL")
		nul, err := windows.CreateFile(nulName, windows.GENERIC_READ,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
			windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			return fail(fmt.Errorf("open NUL for core stdin: %w", err))
		}
		logHandle := windows.Handle(logFile.Fd())
		stdIn, errIn := dup(nul)
		stdOut, errOut := dup(logHandle)
		stdErr, errErr := dup(logHandle)
		windows.CloseHandle(nul)
		dups := []windows.Handle{stdIn, stdOut, stdErr}
		closeDups := func() {
			for _, h := range dups {
				if h != 0 {
					windows.CloseHandle(h)
				}
			}
		}
		if err := errors.Join(errIn, errOut, errErr); err != nil {
			closeDups()
			return fail(fmt.Errorf("prepare core handles: %w", err))
		}

		// Restrict handle inheritance to exactly the three
		// duplicated standard handles. With bInheritHandles=true
		// but no handle list, the child would inherit EVERY
		// inheritable handle this process happens to hold; the
		// attribute list bounds the set to the duplicates.
		attrList, err := windows.NewProcThreadAttributeList(1)
		if err != nil {
			closeDups()
			return fail(fmt.Errorf("prepare core handle list: %w", err))
		}
		defer attrList.Delete()
		if err := attrList.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
			unsafe.Pointer(&dups[0]),
			uintptr(len(dups))*unsafe.Sizeof(dups[0])); err != nil {
			closeDups()
			return fail(fmt.Errorf("prepare core handle list: %w", err))
		}

		parts := make([]string, 0, len(args)+1)
		parts = append(parts, quoteArg(bin))
		for _, a := range args {
			parts = append(parts, quoteArg(a))
		}
		cmdline, err := windows.UTF16PtrFromString(strings.Join(parts, " "))
		if err != nil {
			closeDups()
			return fail(err)
		}
		exePtr, err := windows.UTF16PtrFromString(bin)
		if err != nil {
			closeDups()
			return fail(err)
		}
		dirPtr, err := windows.UTF16PtrFromString(dir)
		if err != nil {
			closeDups()
			return fail(err)
		}
		si := &windows.StartupInfoEx{
			StartupInfo: windows.StartupInfo{
				Cb:        uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
				Flags:     windows.STARTF_USESTDHANDLES,
				StdInput:  stdIn,
				StdOutput: stdOut,
				StdErr:    stdErr,
			},
			ProcThreadAttributeList: attrList.List(),
		}
		pi := &windows.ProcessInformation{}
		// CREATE_SUSPENDED: the process does not run until it is
		// inside the job and ResumeThread is called.
		// EXTENDED_STARTUPINFO_PRESENT selects the StartupInfoEx
		// (with the handle list) over a plain StartupInfo.
		err = windows.CreateProcess(exePtr, cmdline, nil, nil, true,
			windows.CREATE_SUSPENDED|windows.CREATE_NO_WINDOW|windows.EXTENDED_STARTUPINFO_PRESENT,
			nil, dirPtr, &si.StartupInfo, pi)
		closeDups() // the child holds its own copies now (or never started)
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

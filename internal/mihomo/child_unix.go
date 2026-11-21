//go:build !windows

package mihomo

import (
	"os"
	"os/exec"
)

// execChild is the non-Windows childProcess: a plain os/exec
// child. DeBrowser's supported platform is Windows (where the
// Job Object implementation lives); this variant only keeps the
// package buildable and testable elsewhere.
type execChild struct{ cmd *exec.Cmd }

func (c *execChild) Wait() error { return c.cmd.Wait() }

func (c *execChild) Kill() error {
	if c.cmd.Process == nil {
		return nil
	}
	return c.cmd.Process.Kill()
}

func init() {
	startCoreChild = func(bin string, args []string, dir string, logFile *os.File) (childProcess, error) {
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return &execChild{cmd: cmd}, nil
	}
}

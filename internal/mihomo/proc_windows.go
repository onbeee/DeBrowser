package mihomo

import (
	"os/exec"
	"syscall"
)

// setProcAttrs keeps the console core from flashing a console
// window when launched from the GUI application.
func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

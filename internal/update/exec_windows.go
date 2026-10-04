package update

import (
	"os/exec"
	"syscall"
)

// hideWindow keeps child processes from opening a console window when
// mcconv runs as a GUI application.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

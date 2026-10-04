//go:build !windows

package gui

import (
	"errors"
	"os/exec"
)

// Run is only implemented on Windows.
func Run() error {
	return errors.New("the GUI is only available on Windows; usage: mcconv <input world> <output world>")
}

func hideWindow(*exec.Cmd) {}

func totalMemoryMB() int { return 0 }

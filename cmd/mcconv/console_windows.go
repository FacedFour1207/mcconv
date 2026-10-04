package main

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procAttachConsole  = kernel32.NewProc("AttachConsole")
	procGetConsoleList = kernel32.NewProc("GetConsoleProcessList")
	procFreeConsole    = kernel32.NewProc("FreeConsole")
)

// attachConsole makes CLI output visible when the exe is built as a GUI
// application (-H windowsgui) and started from a terminal. Redirected
// handles (pipes, files) are left alone.
func attachConsole() {
	if h, err := syscall.GetStdHandle(syscall.STD_ERROR_HANDLE); err == nil && h != 0 && h != syscall.InvalidHandle {
		return
	}
	const attachParent = ^uintptr(0) // ATTACH_PARENT_PROCESS (-1)
	if r, _, _ := procAttachConsole.Call(attachParent); r == 0 {
		return
	}
	if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stdout = f
		os.Stderr = f
	}
}

// hideOwnConsole closes the console window Windows opens for a console
// build that was started by double-clicking (the GUI does not need it).
func hideOwnConsole() {
	var ids [2]uint32
	n, _, _ := procGetConsoleList.Call(uintptr(unsafe.Pointer(&ids[0])), 2)
	if n == 1 { // only this process uses the console
		procFreeConsole.Call()
	}
}

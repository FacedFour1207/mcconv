//go:build windows

package gui

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// TestPickFolder opens the folder dialog and lets a script type a path:
//
//	MCCONV_DIALOG_TEST=1 go test ./internal/gui -run TestPickFolder
func TestPickFolder(t *testing.T) {
	if os.Getenv("MCCONV_DIALOG_TEST") == "" {
		t.Skip("interactive")
	}
	runtime.LockOSThread()
	want := os.TempDir()
	script := `Start-Sleep 2; $s = New-Object -ComObject WScript.Shell; $null = $s.AppActivate('mcconv dialog test'); Start-Sleep -Milliseconds 500; $s.SendKeys('` + want + `'); Start-Sleep -Milliseconds 300; $s.SendKeys('{ENTER}'); Start-Sleep -Milliseconds 800; $s.SendKeys('{ENTER}')`
	cmd := exec.Command("powershell", "-NoProfile", "-Command", script)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	got := pickFolder(0, "mcconv dialog test", `C:\`)
	cmd.Wait()
	t.Logf("picked %q (want %q)", got, want)
	if got == "" {
		t.Fatal("no folder returned")
	}
}

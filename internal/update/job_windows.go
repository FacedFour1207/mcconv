package update

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var killJob windows.Handle

// killWithParent puts a child process in a job object that ends it when
// mcconv exits (also when mcconv is killed, e.g. a conversion canceled in
// the GUI), so no Minecraft server keeps running.
func killWithParent(p *os.Process) {
	if killJob == 0 {
		h, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			return
		}
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
			windows.CloseHandle(h)
			return
		}
		killJob = h // kept open until mcconv exits
	}
	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(ph)
	windows.AssignProcessToJobObject(killJob, ph)
}

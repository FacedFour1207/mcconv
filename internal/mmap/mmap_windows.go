package mmap

import (
	"os"
	"syscall"
	"unsafe"
)

// Map maps a file read-only into memory.
func Map(f *os.File, size int64) ([]byte, func() error, error) {
	if size == 0 {
		return nil, func() error { return nil }, nil
	}
	h, err := syscall.CreateFileMapping(syscall.Handle(f.Fd()), nil, syscall.PAGE_READONLY, uint32(size>>32), uint32(size), nil)
	if err != nil {
		return nil, nil, err
	}
	addr, err := syscall.MapViewOfFile(h, syscall.FILE_MAP_READ, 0, 0, uintptr(size))
	if err != nil {
		syscall.CloseHandle(h)
		return nil, nil, err
	}
	ptr := *(*unsafe.Pointer)(unsafe.Pointer(&addr)) // mapped view, not Go memory
	data := unsafe.Slice((*byte)(ptr), int(size))
	prefetch(addr, uintptr(size))
	return data, func() error {
		syscall.UnmapViewOfFile(addr)
		return syscall.CloseHandle(h)
	}, nil
}

var noPrefetch = os.Getenv("MCCONV_NOPREFETCH") != ""

var procPrefetch = syscall.NewLazyDLL("kernel32.dll").NewProc("PrefetchVirtualMemory")

// prefetch asks Windows to bring the whole view in at once, which avoids a
// page fault per 4 KiB page when chunks are read in random order.
func prefetch(addr, size uintptr) {
	if noPrefetch || procPrefetch.Find() != nil {
		return
	}
	type rangeEntry struct {
		addr uintptr
		size uintptr
	}
	r := rangeEntry{addr, size}
	cur, _ := syscall.GetCurrentProcess()
	procPrefetch.Call(uintptr(cur), 1, uintptr(unsafe.Pointer(&r)), 0)
}

//go:build !windows

package mmap

import (
	"io"
	"os"
)

// Map reads the whole file (memory mapping is only used on Windows).
func Map(f *os.File, size int64) ([]byte, func() error, error) {
	data := make([]byte, size)
	_, err := io.ReadFull(f, data)
	return data, func() error { return nil }, err
}

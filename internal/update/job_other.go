//go:build !windows

package update

import "os"

func killWithParent(*os.Process) {}

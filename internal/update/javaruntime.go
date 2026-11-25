package update

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
)

// Mojang's Java runtimes (the ones the official launcher installs), used
// when no suitable Java is installed.
const javaRuntimeIndex = "https://launchermeta.mojang.com/v1/products/java-runtime/2ec0cc96c44e5a76b9c8b7c39df7210883d12871/all.json"

func runtimePlatform() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/amd64":
		return "windows-x64"
	case "windows/arm64":
		return "windows-arm64"
	case "windows/386":
		return "windows-x86"
	case "darwin/arm64":
		return "mac-os-arm64"
	case "darwin/amd64":
		return "mac-os"
	case "linux/386":
		return "linux-i386"
	}
	return "linux"
}

// ownRuntimeJava is the java executable of a runtime downloaded by mcconv.
func ownRuntimeJava(dataDir, component string) string {
	exe := "java"
	if runtime.GOOS == "windows" {
		exe = "java.exe"
	}
	return filepath.Join(dataDir, "runtime", component, "bin", exe)
}

type runtimeFile struct {
	Type       string `json:"type"` // file, directory, link
	Executable bool   `json:"executable"`
	Target     string `json:"target"`
	Downloads  struct {
		Raw struct {
			SHA1 string `json:"sha1"`
			Size int64  `json:"size"`
			URL  string `json:"url"`
		} `json:"raw"`
	} `json:"downloads"`
}

// downloadRuntime installs Mojang's Java runtime <component> into
// dataDir/runtime/<component>.
func downloadRuntime(dataDir, component string, progress func(done, total int64)) error {
	b, err := httpGet(javaRuntimeIndex)
	if err != nil {
		return err
	}
	var index map[string]map[string][]struct {
		Manifest struct {
			SHA1 string `json:"sha1"`
			URL  string `json:"url"`
		} `json:"manifest"`
		Version struct {
			Name string `json:"name"`
		} `json:"version"`
	}
	if err := json.Unmarshal(b, &index); err != nil {
		return err
	}
	l := index[runtimePlatform()][component]
	if len(l) == 0 {
		return fmt.Errorf("Mojang has no Java runtime %s for %s", component, runtimePlatform())
	}
	mb, err := httpGet(l[0].Manifest.URL)
	if err != nil {
		return err
	}
	if sha1Hex(mb) != l[0].Manifest.SHA1 {
		return fmt.Errorf("%s: checksum mismatch", l[0].Manifest.URL)
	}
	var man struct {
		Files map[string]runtimeFile `json:"files"`
	}
	if err := json.Unmarshal(mb, &man); err != nil {
		return err
	}
	final := filepath.Join(dataDir, "runtime", component)
	tmp := final + ".part"
	os.RemoveAll(tmp)
	var paths []string
	var total int64
	for p, f := range man.Files {
		switch f.Type {
		case "directory":
			if err := os.MkdirAll(filepath.Join(tmp, filepath.FromSlash(p)), 0o755); err != nil {
				return err
			}
		case "file":
			paths = append(paths, p)
			total += f.Downloads.Raw.Size
		}
	}
	sort.Strings(paths)
	var (
		done     atomic.Int64
		firstErr error
		errOnce  sync.Once
		wg       sync.WaitGroup
		next     atomic.Int64
	)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(paths) {
					return
				}
				f := man.Files[paths[i]]
				dst := filepath.Join(tmp, filepath.FromSlash(paths[i]))
				var last int64
				err := downloadChecked(f.Downloads.Raw.URL, dst, f.Downloads.Raw.SHA1, func(n int64) {
					progress(done.Add(n-last), total)
					last = n
				})
				if err == nil && f.Executable && runtime.GOOS != "windows" {
					err = os.Chmod(dst, 0o755)
				}
				if err != nil {
					errOnce.Do(func() { firstErr = err })
					return
				}
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		os.RemoveAll(tmp)
		return firstErr
	}
	if runtime.GOOS != "windows" {
		for p, f := range man.Files {
			if f.Type == "link" {
				os.Symlink(f.Target, filepath.Join(tmp, filepath.FromSlash(p)))
			}
		}
	}
	os.RemoveAll(final)
	return os.Rename(tmp, final)
}

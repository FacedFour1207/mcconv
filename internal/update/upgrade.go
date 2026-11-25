package update

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Old Java worlds (saved before the oldest format mcconv reads) are
// upgraded with the official Minecraft server: a copy of the world is
// opened with --forceUpgrade, which runs the game's own data fixers on every
// chunk. The original world is not changed.

const versionManifest = "https://piston-meta.mojang.com/mc/game/version_manifest_v2.json"

// UpgradeOptions configures UpgradeWorld.
type UpgradeOptions struct {
	DataDir string // cache for the server jar and the Java runtime
	Version string // game version whose server upgrades the world ("26.3")
	WorkDir string // folder for the copy; created, must not exist
	Logf    func(format string, args ...any)
	// Progress reports a step: "server" (downloading the server, bytes),
	// "java" (downloading Java, bytes), "copy" (copying the world, bytes)
	// or "upgrade" (chunks).
	Progress func(step string, done, total int64)
}

func (o *UpgradeOptions) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

func (o *UpgradeOptions) progress(step string, done, total int64) {
	if o.Progress != nil {
		o.Progress(step, done, total)
	}
}

// serverVersion is the part of a version JSON the upgrade needs.
type serverVersion struct {
	Downloads struct {
		Server struct {
			SHA1 string `json:"sha1"`
			Size int64  `json:"size"`
			URL  string `json:"url"`
		} `json:"server"`
	} `json:"downloads"`
	JavaVersion struct {
		Component    string `json:"component"`
		MajorVersion int    `json:"majorVersion"`
	} `json:"javaVersion"`
}

// UpgradeWorld copies the Java world src to o.WorkDir/world and upgrades
// the copy with the official server of o.Version. It returns the folder of
// the upgraded copy. The caller removes o.WorkDir.
func UpgradeWorld(src string, o UpgradeOptions) (string, error) {
	if a, err := filepath.Abs(o.DataDir); err == nil {
		o.DataDir = a // the server runs in its own folder
	}
	sdir := filepath.Join(o.DataDir, "server", o.Version)
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		return "", err
	}
	v, err := loadServerVersion(o.Version, sdir)
	if err != nil {
		return "", err
	}
	jar := filepath.Join(sdir, "server.jar")
	if fileSHA1(jar) != v.Downloads.Server.SHA1 {
		o.logf("downloading the official Minecraft %s server (%.0f MB)", o.Version, float64(v.Downloads.Server.Size)/(1<<20))
		err := downloadChecked(v.Downloads.Server.URL, jar, v.Downloads.Server.SHA1, func(done int64) {
			o.progress("server", done, v.Downloads.Server.Size)
		})
		if err != nil {
			return "", fmt.Errorf("download the Minecraft server: %w", err)
		}
	}
	java, err := serverJava(o.DataDir, v.JavaVersion.Component, v.JavaVersion.MajorVersion, &o)
	if err != nil {
		return "", err
	}
	o.logf("Java: %s", java)

	world := filepath.Join(o.WorkDir, "world")
	if err := os.MkdirAll(o.WorkDir, 0o755); err != nil {
		return "", err
	}
	o.logf("copying the world to %s", world)
	if err := copyWorld(src, world, func(done, total int64) { o.progress("copy", done, total) }); err != nil {
		return "", fmt.Errorf("copy the world: %w", err)
	}
	// Chunks kept loaded with /forceload would be loaded (and partly
	// generated anew) when the server starts after the upgrade.
	filepath.WalkDir(world, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "chunks.dat" && filepath.Base(filepath.Dir(p)) == "data" {
			os.Remove(p)
		}
		return nil
	})
	if err := runUpgrade(java, sdir, o.WorkDir, &o); err != nil {
		return "", err
	}
	return world, nil
}

// loadServerVersion reads the version JSON of a release from Mojang (and
// keeps a copy, used when there is no connection).
func loadServerVersion(version, dir string) (*serverVersion, error) {
	cache := filepath.Join(dir, "version.json")
	raw, err := fetchVersionJSON(version)
	if err != nil {
		b, cerr := os.ReadFile(cache)
		if cerr != nil {
			return nil, fmt.Errorf("Minecraft %s version information: %w", version, err)
		}
		raw = b
	} else {
		os.WriteFile(cache, raw, 0o644)
	}
	var v serverVersion
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	if v.Downloads.Server.URL == "" || v.Downloads.Server.SHA1 == "" {
		return nil, fmt.Errorf("Minecraft %s has no server download", version)
	}
	return &v, nil
}

func fetchVersionJSON(version string) ([]byte, error) {
	b, err := httpGet(versionManifest)
	if err != nil {
		return nil, err
	}
	var m struct {
		Versions []struct {
			ID   string `json:"id"`
			URL  string `json:"url"`
			SHA1 string `json:"sha1"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for _, v := range m.Versions {
		if v.ID == version {
			b, err := httpGet(v.URL)
			if err != nil {
				return nil, err
			}
			if v.SHA1 != "" && sha1Hex(b) != v.SHA1 {
				return nil, fmt.Errorf("%s: checksum mismatch", v.URL)
			}
			return b, nil
		}
	}
	return nil, fmt.Errorf("version %s is not in the Minecraft version list", version)
}

func httpGet(url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

var bigClient = &http.Client{} // large files: no overall timeout

// downloadChecked downloads url to dst (through a temporary file) and
// checks its SHA-1.
func downloadChecked(url, dst, sum string, progress func(done int64)) error {
	resp, err := bigClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha1.New()
	var done int64
	buf := make([]byte, 256<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				f.Close()
				os.Remove(tmp)
				return err
			}
			h.Write(buf[:n])
			done += int64(n)
			if progress != nil {
				progress(done)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(tmp)
			return rerr
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); sum != "" && got != sum {
		os.Remove(tmp)
		return fmt.Errorf("%s: checksum mismatch", url)
	}
	os.Remove(dst)
	return os.Rename(tmp, dst)
}

func fileSHA1(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sha1Hex(b []byte) string {
	s := sha1.Sum(b)
	return hex.EncodeToString(s[:])
}

// serverJava finds a Java runtime for the server (java.exe, which keeps
// the console streams), downloading Mojang's runtime if there is none.
func serverJava(dataDir, component string, major int, o *UpgradeOptions) (string, error) {
	own := ownRuntimeJava(dataDir, component)
	if javaMajor(own) >= major {
		return own, nil
	}
	if p, err := findJavaRuntime(component, major); err == nil {
		if c := filepath.Join(filepath.Dir(p), "java.exe"); strings.EqualFold(filepath.Base(p), "javaw.exe") && fileExists(c) {
			p = c
		}
		return p, nil
	}
	o.logf("no Java %d found: downloading the official Java runtime (%s) from Mojang", major, component)
	if err := downloadRuntime(dataDir, component, func(done, total int64) { o.progress("java", done, total) }); err != nil {
		return "", fmt.Errorf("download Java: %w", err)
	}
	if javaMajor(own) < major {
		return "", fmt.Errorf("the downloaded Java runtime is not Java %d", major)
	}
	return own, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// copyWorld copies a world folder (without session.lock).
func copyWorld(src, dst string, progress func(done, total int64)) error {
	var total int64
	filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && d.Name() != "session.lock" {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	var done int64
	defer func() { progress(total, total) }() // the last call may be throttled away
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		if d.Name() == "session.lock" || !d.Type().IsRegular() {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		f, err := os.Create(out)
		if err != nil {
			return err
		}
		n, err := io.Copy(f, in)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		done += n
		progress(done, total)
		return err
	})
}

var (
	reChunks   = regexp.MustCompile(`(\d+)% completed \((\d+) / (\d+) chunks\)`)
	reFinished = regexp.MustCompile(`World optimization finished`)
)

// runUpgrade runs the server in sdir (where it unpacks its libraries once)
// on workDir/world with --forceUpgrade and stops it once it has started.
func runUpgrade(java, sdir, workDir string, o *UpgradeOptions) error {
	port, err := freePort()
	if err != nil {
		return err
	}
	// written for each run: the user accepted the EULA for this conversion
	if err := os.WriteFile(filepath.Join(sdir, "eula.txt"), []byte("eula=true\n"), 0o644); err != nil {
		return err
	}
	props := fmt.Sprintf("level-name=world\nserver-ip=127.0.0.1\nserver-port=%d\nonline-mode=false\nenable-query=false\nenable-rcon=false\nmax-tick-time=-1\n", port)
	if err := os.WriteFile(filepath.Join(sdir, "server.properties"), []byte(props), 0o644); err != nil {
		return err
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return err
	}
	// Java on Windows makes a Unix domain socket file for its internal pipes
	// in the temp folder; where that fails ("Unable to establish loopback
	// connection") the server cannot start, so use a folder of our own.
	sock := filepath.Join(sdir, "tmp")
	if a, err := filepath.Abs(sock); err == nil {
		sock = a
	}
	if os.MkdirAll(sock, 0o755) != nil {
		sock = os.TempDir()
	}
	cmd := exec.Command(java, "-Djdk.net.unixdomain.tmpdir="+sock, "-jar", "server.jar",
		"--forceUpgrade", "--eraseCache", "--nogui", "--universe", abs, "--world", "world")
	cmd.Dir = sdir
	hideWindow(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	o.logf("upgrading the world with the official Minecraft server (--forceUpgrade)")
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the Minecraft server: %w", err)
	}
	killWithParent(cmd.Process) // also stopped if mcconv is stopped
	var (
		mu       sync.Mutex
		tail     []string
		finished bool
		started  bool
	)
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		mu.Lock()
		tail = append(tail, line)
		if len(tail) > 15 {
			tail = tail[1:]
		}
		mu.Unlock()
		if m := reChunks.FindStringSubmatch(line); m != nil {
			d, _ := strconv.ParseInt(m[2], 10, 64)
			n, _ := strconv.ParseInt(m[3], 10, 64)
			o.progress("upgrade", d, n)
		}
		if reFinished.MatchString(line) {
			finished = true
			o.logf("upgrade finished in %v", time.Since(start).Round(time.Second))
		}
		if !started && strings.Contains(line, "Done (") {
			started = true
			io.WriteString(stdin, "stop\n") // saves level.dat and exits
		}
	}
	err = cmd.Wait()
	if !finished {
		if err == nil {
			err = errors.New("the server stopped before the upgrade finished")
		}
		return fmt.Errorf("upgrade with the Minecraft server failed: %v\n%s", err, strings.Join(tail, "\n"))
	}
	if !started || err != nil {
		return fmt.Errorf("the Minecraft server did not stop normally after the upgrade (%v)\n%s", err, strings.Join(tail, "\n"))
	}
	return nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

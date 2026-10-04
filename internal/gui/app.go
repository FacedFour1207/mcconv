// Package gui is the graphical front end: a WebView2 window whose page
// (ui/index.html) calls the methods of App. Conversions run in a child
// process of the same executable (mcconv -progress-json ...), so every run
// starts with fresh settings and can be stopped at once.
package gui

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"mcconv/internal/config"
	"mcconv/internal/translate"
)

// App holds the state behind the window.
type App struct {
	exe     string
	dataDir string
	emit    func(ev map[string]any) // delivers an event to the page (any goroutine)

	mu  sync.Mutex
	job *job
}

type job struct {
	cmd      *exec.Cmd
	out      string // output world ("" for update-data)
	canceled bool
}

func newApp(emit func(map[string]any)) *App {
	exe, _ := os.Executable()
	return &App{exe: exe, dataDir: config.DefaultDir(), emit: emit}
}

// State is what the page needs at start-up.
type State struct {
	Workers  int      `json:"workers"`
	MemoryMB int      `json:"memoryMB"` // installed RAM
	DataDir  string   `json:"dataDir"`
	Settings Versions `json:"settings"`
}

// Versions are the editable target versions.
type Versions struct {
	JavaDataVersion int    `json:"javaDataVersion"`
	JavaName        string `json:"javaName"`
	BedrockVersion  string `json:"bedrockVersion"`
}

// State returns the initial state and starts loading the table summary.
func (a *App) State() State {
	go a.tablesSummary()
	go a.versionLists()
	return State{Workers: runtime.NumCPU(), MemoryMB: totalMemoryMB(), DataDir: a.dataDir, Settings: a.versions()}
}

func (a *App) versions() Versions {
	cfg, err := config.Load(a.dataDir)
	if err != nil {
		cfg = &config.Config{Versions: config.DefaultVersions()}
	}
	v := cfg.Versions
	parts := make([]string, len(v.BedrockVersion))
	for i, n := range v.BedrockVersion {
		parts[i] = strconv.Itoa(int(n))
	}
	return Versions{JavaDataVersion: v.JavaDataVersion, JavaName: v.JavaName, BedrockVersion: strings.Join(parts, ".")}
}

// tablesSummary reports which translation tables conversions will use.
func (a *App) tablesSummary() {
	src, name := translate.BuiltinTables(), "内蔵データ"
	if p := filepath.Join(a.dataDir, "tables.json.gz"); fileExists(p) {
		if b, err := os.ReadFile(p); err == nil {
			src, name = b, "mcconv-data\tables.json.gz"
		}
	}
	t, err := translate.Parse(src)
	if err != nil {
		a.emit(map[string]any{"t": "tables", "error": err.Error()})
		return
	}
	a.emit(map[string]any{"t": "tables", "source": name, "java": t.JavaVersion, "bedrock": t.BedrockVersion})
}

// SaveVersions writes versions.json in the data directory.
func (a *App) SaveVersions(v Versions) error {
	cfg, err := config.Load(a.dataDir)
	if err != nil {
		return err
	}
	if v.JavaDataVersion <= 0 || strings.TrimSpace(v.JavaName) == "" {
		return errors.New("Java のバージョンを入力してください")
	}
	var bv []int32
	for _, p := range strings.Split(strings.TrimSpace(v.BedrockVersion), ".") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 {
			return fmt.Errorf("統合版のバージョン %q は 1.26.52.3 の形式で入力してください", v.BedrockVersion)
		}
		bv = append(bv, int32(n))
	}
	for len(bv) < 4 {
		bv = append(bv, 0)
	}
	cfg.Versions.JavaDataVersion = v.JavaDataVersion
	cfg.Versions.JavaName = strings.TrimSpace(v.JavaName)
	cfg.Versions.BedrockVersion = bv[:4]
	if err := os.MkdirAll(a.dataDir, 0o755); err != nil {
		return err
	}
	return config.WriteJSON(filepath.Join(a.dataDir, "versions.json"), cfg.Versions)
}

// Worlds lists the worlds of both editions and measures their sizes in the
// background ("size" events).
func (a *App) Worlds() map[string][]WorldInfo {
	j, b := listWorlds()
	go func() {
		for _, l := range [][]WorldInfo{j, b} {
			for _, w := range l {
				a.emit(map[string]any{"t": "size", "path": w.Path, "size": dirSize(w.Path)})
			}
		}
	}()
	return map[string][]WorldInfo{"java": j, "bedrock": b}
}

// Inspect reads a world chosen with the folder dialog.
func (a *App) Inspect(dir string) (*WorldInfo, error) {
	w, err := inspectWorld(dir)
	if err != nil {
		return nil, errors.New("このフォルダは Minecraft のワールドではありません (level.dat がありません)")
	}
	go func() { a.emit(map[string]any{"t": "size", "path": w.Path, "size": dirSize(w.Path)}) }()
	return w, nil
}

// Destinations lists where a converted world can be saved.
func (a *App) Destinations(target string) []Destination { return destinations(target) }

// StartRequest is a conversion started from the page.
type StartRequest struct {
	Source      string   `json:"source"`
	Target      string   `json:"target"` // "java" | "bedrock"
	Name        string   `json:"name"`
	DestRoot    string   `json:"destRoot"`
	Dims        []string `json:"dims"`
	Workers     int      `json:"workers"`
	Compression int      `json:"compression"`
	AllChunks   bool     `json:"allChunks"`
	MemoryMB    int      `json:"memoryMB"` // 0 = no limit
}

// Start begins a conversion and returns the output folder. Progress arrives
// as events: log, p (progress), done / error, and finally exit.
func (a *App) Start(r StartRequest) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.job != nil {
		return "", errors.New("別の処理を実行中です")
	}
	if r.Source == "" {
		return "", errors.New("変換するワールドを選んでください")
	}
	if r.DestRoot == "" {
		return "", errors.New("保存先を選んでください")
	}
	if len(r.Dims) == 0 {
		return "", errors.New("ディメンションを 1 つ以上選んでください")
	}
	name := strings.TrimSpace(r.Name)
	if name == "" {
		name = "Converted World"
	}
	out, err := outputDir(r.DestRoot, r.Target, name)
	if err != nil {
		return "", err
	}
	if abs, _ := filepath.Abs(r.Source); strings.EqualFold(abs, out) {
		return "", errors.New("入力と出力が同じフォルダです")
	}
	args := []string{"-progress-json", "-v", "-to", r.Target, "-dims", strings.Join(r.Dims, ","),
		"-compression", strconv.Itoa(r.Compression), "-name", name, "-data", a.dataDir}
	if r.Workers > 0 {
		args = append(args, "-workers", strconv.Itoa(r.Workers))
	}
	if r.MemoryMB > 0 {
		args = append(args, "-mem", strconv.Itoa(r.MemoryMB))
	}
	if r.AllChunks {
		args = append(args, "-all-chunks")
	}
	args = append(args, r.Source, out)
	return out, a.run(&job{out: out}, args, false)
}

// UpdateData runs "mcconv update-data" (refresh tables after a game update).
func (a *App) UpdateData() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.job != nil {
		return errors.New("別の処理を実行中です")
	}
	return a.run(&job{}, []string{"update-data", "-data", a.dataDir}, true)
}

// run starts a child process; must be called with a.mu held.
func (a *App) run(j *job, args []string, update bool) error {
	cmd := exec.Command(a.exe, args...)
	hideWindow(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		if j.out != "" {
			os.RemoveAll(j.out)
		}
		return err
	}
	j.cmd = cmd
	a.job = j
	logType := "log"
	if update {
		logType = "ulog"
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // JSON events (conversion) or plain text (update-data)
		defer wg.Done()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			var ev map[string]any
			if !update && json.Unmarshal(sc.Bytes(), &ev) == nil {
				a.emit(ev)
			} else {
				a.emit(map[string]any{"t": logType, "m": sc.Text()})
			}
		}
	}()
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			a.emit(map[string]any{"t": logType, "m": sc.Text()})
		}
	}()
	go func() {
		wg.Wait()
		err := cmd.Wait()
		a.mu.Lock()
		canceled := j.canceled
		a.job = nil
		a.mu.Unlock()
		ok := err == nil && !canceled
		if !ok && j.out != "" {
			os.RemoveAll(j.out) // never leave a half-written world behind
		}
		ev := map[string]any{"t": "exit", "ok": ok, "canceled": canceled, "update": update}
		if err != nil && !canceled {
			ev["m"] = err.Error()
		}
		a.emit(ev)
		if update && ok {
			a.tablesSummary()
		}
	}()
	return nil
}

// Cancel stops the running job.
func (a *App) Cancel() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.job != nil && a.job.cmd.Process != nil {
		a.job.canceled = true
		a.job.cmd.Process.Kill()
	}
}

// shutdown stops a running job when the window closes and waits until its
// partial output is removed.
func (a *App) shutdown() {
	a.mu.Lock()
	j := a.job
	a.mu.Unlock()
	if j == nil {
		return
	}
	a.Cancel()
	j.cmd.Process.Wait()
	if j.out != "" {
		os.RemoveAll(j.out)
	}
}

// outputDir picks a new, not yet existing folder below root.
func outputDir(root, target, name string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return "", fmt.Errorf("保存先 %s が見つかりません", root)
	}
	if target == "bedrock" && filepath.Base(root) == "minecraftWorlds" {
		// the game names world folders with random ids
		for {
			var b [8]byte
			rand.Read(b[:])
			p := filepath.Join(root, base64.URLEncoding.EncodeToString(b[:])[:11]+"=")
			if !fileExists(p) {
				return p, nil
			}
		}
	}
	base := sanitize(name)
	p := filepath.Join(root, base)
	for i := 2; fileExists(p); i++ {
		p = filepath.Join(root, fmt.Sprintf("%s (%d)", base, i))
	}
	return p, nil
}

func sanitize(name string) string {
	s := strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	s = strings.TrimRight(strings.TrimSpace(s), ". ")
	if s == "" {
		s = "world"
	}
	return s
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

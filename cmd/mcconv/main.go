// Command mcconv converts Minecraft worlds between Java Edition and Bedrock Edition.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"runtime/trace"
	"strings"
	"sync"
	"time"

	"mcconv/internal/config"
	"mcconv/internal/convert"
	"mcconv/internal/gui"
	"mcconv/internal/java"
	"mcconv/internal/translate"
	"mcconv/internal/update"
	"mcconv/internal/world"
)

func main() {
	if len(os.Args) == 1 {
		// double-clicked: open the GUI
		hideOwnConsole()
		if err := gui.Run(); err != nil {
			fatal(err)
		}
		return
	}
	attachConsole()
	if len(os.Args) > 1 && os.Args[1] == "update-data" {
		updateData(os.Args[2:])
		return
	}
	var (
		to         = flag.String("to", "", "target edition: bedrock | java (default: the opposite of the input)")
		dims       = flag.String("dims", "overworld,nether,end", "dimensions to convert")
		workers    = flag.Int("workers", runtime.GOMAXPROCS(0), "number of parallel workers")
		level      = flag.Int("compression", 1, "compression level -2..9 (0 = none, -2 = Huffman only; default 1)")
		unfinished = flag.Bool("all-chunks", false, "also convert chunks that are not fully generated")
		quiet      = flag.Bool("q", false, "no progress output")
		dataDir    = flag.String("data", config.DefaultDir(), "data directory with updated tables/versions (see update-data)")
		cpuProf    = flag.String("cpuprofile", "", "write a CPU profile (for development)")
		verbose    = flag.Bool("v", false, "show timing of each phase")
		traceFile  = flag.String("trace", "", "write an execution trace (for development)")
		name       = flag.String("name", "", "name of the output world (default: the input world's name)")
		jsonOut    = flag.Bool("progress-json", false, "report progress as JSON lines on stdout (used by the GUI)")
		memMB      = flag.Int("mem", 0, "memory limit in MB (0 = none; lower uses less RAM but converts slower)")
		javaVer    = flag.String("java-version", "", "Java version to write, 1.21.1 or newer (default: versions.json, else the tables' version)")
		upgradeOld = flag.Bool("upgrade-old", false, "Java worlds older than 1.21.1: first upgrade a copy with the official Minecraft server (--forceUpgrade; downloads the server, and Java if none is found). Needs -accept-eula")
		acceptEULA = flag.Bool("accept-eula", false, "accept the Minecraft EULA (https://aka.ms/MinecraftEULA), needed to run the official server for -upgrade-old")
		replace    = flag.String("replace", "", "replace blocks: from=to,from=to (source edition id = target edition id, e.g. barrier=invisible_bedrock; add-on / mod ids may have states: stone=addon:custom_block[stage=1])")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: mcconv [options] <input world> <output world>\n")
		fmt.Fprintf(os.Stderr, "       mcconv update-data [-data DIR] [-java DIR] [-bedrock DIR]   (refresh data after a game update)\n\n")
		fmt.Fprintf(os.Stderr, "Converts a Java Edition world to Bedrock Edition or vice versa.\n")
		fmt.Fprintf(os.Stderr, "The input edition is detected automatically.\n\noptions:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}
	in, out := flag.Arg(0), flag.Arg(1)
	from, err := convert.Detect(in)
	if err != nil {
		fatal(err)
	}
	target := *to
	if target == "" {
		if from == "java" {
			target = "bedrock"
		} else {
			target = "java"
		}
	}
	if target == from {
		fatal(fmt.Errorf("input is already a %s world", from))
	}
	if *cpuProf != "" {
		f, err := os.Create(*cpuProf)
		if err != nil {
			fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
		// MCCONV_PROFILE_SECS=N profiles only the first N seconds (long runs)
		if secs, err := time.ParseDuration(os.Getenv("MCCONV_PROFILE_SECS") + "s"); err == nil && secs > 0 {
			time.AfterFunc(secs, func() { pprof.StopCPUProfile(); f.Close() })
		}
	}
	if *traceFile != "" {
		f, err := os.Create(*traceFile)
		if err != nil {
			fatal(err)
		}
		trace.Start(f)
		defer trace.Stop()
	}
	// conversion allocates many short-lived objects; trade memory for less GC
	debug.SetGCPercent(400)
	if *memMB > 0 {
		// the garbage collector works harder to stay below the limit
		debug.SetMemoryLimit(int64(*memMB) << 20)
	}
	cfg, err := config.Load(*dataDir)
	if err != nil {
		fatal(err)
	}
	if *javaVer != "" {
		if p, ok := cfg.File("tables.json.gz"); ok {
			translate.SetDataFile(p)
		}
		t, err := translate.Load()
		if err != nil {
			fatal(err)
		}
		r, ok := t.JavaReleaseByName(*javaVer)
		if !ok {
			var names []string
			for _, r := range t.JavaReleases() {
				names = append(names, r.Name)
			}
			fatal(fmt.Errorf("unknown Java version %q (choose one of: %s)", *javaVer, strings.Join(names, ", ")))
		}
		cfg.Versions.JavaName, cfg.Versions.JavaDataVersion = r.Name, r.DataVersion
	}
	opts := convert.Options{
		Config:            cfg,
		Verbose:           *verbose,
		Workers:           *workers,
		CompressionLevel:  *level,
		IncludeUnfinished: *unfinished,
		LevelName:         *name,
	}
	if opts.Replace, err = convert.ParseReplace(*replace); err != nil {
		fatal(err)
	}
	for _, d := range strings.Split(*dims, ",") {
		switch strings.TrimSpace(strings.ToLower(d)) {
		case "overworld", "0":
			opts.Dimensions = append(opts.Dimensions, world.Overworld)
		case "nether", "the_nether", "-1", "1":
			opts.Dimensions = append(opts.Dimensions, world.Nether)
		case "end", "the_end", "2":
			opts.Dimensions = append(opts.Dimensions, world.End)
		case "":
		default:
			fatal(fmt.Errorf("unknown dimension %q", d))
		}
	}
	if from == "java" {
		var upErr error
		in, upErr = upgradeIfOld(in, out, *upgradeOld, *acceptEULA, cfg.Dir, *jsonOut, *quiet)
		if upErr != nil {
			if *jsonOut {
				jsonFatal(upErr)
			}
			fatal(upErr)
		}
	}
	if *jsonOut {
		runJSON(in, out, from, opts)
		cleanup()
		return
	}
	var mu sync.Mutex
	lastPrint := time.Time{}
	progressLine := false
	opts.Logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if progressLine {
			fmt.Fprint(os.Stderr, "\n")
			progressLine = false
		}
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
	if !*quiet {
		opts.Progress = func(done, total int64) {
			mu.Lock()
			defer mu.Unlock()
			if time.Since(lastPrint) < 200*time.Millisecond && done != total {
				return
			}
			lastPrint = time.Now()
			if total > 0 {
				fmt.Fprintf(os.Stderr, "\r%d / %d chunks (%.0f%%)   ", done, total, float64(done)*100/float64(total))
			} else {
				fmt.Fprintf(os.Stderr, "\r%d chunks   ", done)
			}
			progressLine = true
		}
	}
	fmt.Fprintf(os.Stderr, "%s -> %s: %s -> %s\n", from, target, in, out)
	var st *convert.Stats
	if from == "java" {
		st, err = convert.JavaToBedrock(in, out, opts)
	} else {
		st, err = convert.BedrockToJava(in, out, opts)
	}
	if progressLine {
		fmt.Fprint(os.Stderr, "\n")
	}
	if err != nil {
		fatal(err)
	}
	cleanup()
	fmt.Fprintf(os.Stderr, "done in %v: %d chunks converted, %d skipped, %d failed, %d block entities, %d entities\n",
		st.Duration.Round(time.Millisecond), st.Chunks, st.Skipped, st.Failed, st.BlockEntities, st.Entities)
	if st.TranslateErrs > 0 {
		fmt.Fprintf(os.Stderr, "%d block translation warnings (see above)\n", st.TranslateErrs)
	}
}

func fatal(err error) {
	cleanup()
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

// cleanups run before mcconv exits (the upgraded copy of an old world).
var cleanups []func()

func cleanup() {
	for _, f := range cleanups {
		f()
	}
	cleanups = nil
}

func updateData(args []string) {
	fs := flag.NewFlagSet("update-data", flag.ExitOnError)
	dataDir := fs.String("data", config.DefaultDir(), "data directory to write")
	javaDir := fs.String("java", "", `.minecraft folder (default: %APPDATA%\.minecraft)`)
	bedrockDir := fs.String("bedrock", "", "Bedrock install Content folder (default: auto-detect)")
	offline := fs.Bool("offline", false, "do not download new block tables")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: mcconv update-data [options]\n\n")
		fmt.Fprintf(os.Stderr, "Reads the installed Java/Bedrock games, downloads the newest block mappings\n")
		fmt.Fprintf(os.Stderr, "(GeyserMC) and writes versions.json, bedrock_entities.json and tables.json.gz.\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if err := update.Run(update.Options{DataDir: *dataDir, MinecraftDir: *javaDir, BedrockDir: *bedrockDir, Offline: *offline, Log: os.Stderr}); err != nil {
		fatal(err)
	}
}

// runJSON converts with machine readable output: one JSON object per line
// ({"t":"log"|"p"|"done"|"error", ...}).
func runJSON(in, out, from string, opts convert.Options) {
	var mu sync.Mutex
	enc := json.NewEncoder(os.Stdout)
	emit := func(v map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		enc.Encode(v)
	}
	opts.Logf = func(format string, args ...any) {
		emit(map[string]any{"t": "log", "m": fmt.Sprintf(format, args...)})
	}
	var last time.Time
	opts.Progress = func(done, total int64) {
		mu.Lock()
		skip := time.Since(last) < 100*time.Millisecond && done != total
		if !skip {
			last = time.Now()
		}
		mu.Unlock()
		if !skip {
			emit(map[string]any{"t": "p", "d": done, "n": total})
		}
	}
	var st *convert.Stats
	var err error
	if from == "java" {
		st, err = convert.JavaToBedrock(in, out, opts)
	} else {
		st, err = convert.BedrockToJava(in, out, opts)
	}
	if err != nil {
		emit(map[string]any{"t": "error", "m": err.Error()})
		cleanup()
		os.Exit(1)
	}
	emit(map[string]any{"t": "done", "chunks": st.Chunks, "skipped": st.Skipped, "failed": st.Failed,
		"blockEntities": st.BlockEntities, "entities": st.Entities, "ms": st.Duration.Milliseconds(), "replaced": st.Replaced})
}

// jsonEmit writes one JSON event line (-progress-json).
var jsonMu sync.Mutex

func jsonEmit(v map[string]any) {
	jsonMu.Lock()
	defer jsonMu.Unlock()
	json.NewEncoder(os.Stdout).Encode(v)
}

func jsonFatal(err error) {
	jsonEmit(map[string]any{"t": "error", "m": err.Error()})
	cleanup()
	os.Exit(1)
}

// upgradeIfOld returns the world to convert: in itself, or for a Java world
// older than the oldest format mcconv reads (1.21.1), a copy upgraded by the
// official Minecraft server next to the output (removed by cleanup).
func upgradeIfOld(in, out string, allowed, eula bool, dataDir string, jsonOut, quiet bool) (string, error) {
	w, err := java.Open(in)
	if err != nil {
		return in, nil // reported by the conversion
	}
	dv := w.ReadDataVersion() // 0: before 1.9 (no DataVersion)
	if dv >= translate.MinJavaDataVersion {
		return in, nil
	}
	ver := "before 1.9"
	if dv > 0 {
		ver = fmt.Sprintf("DataVersion %d", dv)
	}
	if !allowed {
		return "", fmt.Errorf("this world was saved by an old Java Edition (%s); mcconv reads 1.21.1 and newer directly. Add -upgrade-old -accept-eula to upgrade a copy with the official Minecraft server first (the original world is not changed)", ver)
	}
	if !eula {
		return "", fmt.Errorf("upgrading the world runs the official Minecraft server, which needs the Minecraft EULA (https://aka.ms/MinecraftEULA): add -accept-eula if you accept it")
	}
	if dataDir == "" {
		dataDir = config.DefaultDir()
	}
	work := strings.TrimRight(out, `/\`) + ".mcconv-upgrade"
	os.RemoveAll(work) // left by a canceled run
	cleanups = append(cleanups, func() { removeAllRetry(work) })
	logf := func(format string, args ...any) {
		if jsonOut {
			jsonEmit(map[string]any{"t": "log", "m": fmt.Sprintf(format, args...)})
		} else {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		}
	}
	logf("this world was saved by an old Java Edition (%s): upgrading a copy with the official Minecraft %s server first", ver, java.ReadFormatName)
	var last time.Time
	lastStep := ""
	progress := func(step string, done, total int64) {
		if time.Since(last) < 150*time.Millisecond && done != total && step == lastStep {
			return
		}
		last, lastStep = time.Now(), step
		if jsonOut {
			jsonEmit(map[string]any{"t": "step", "s": step, "d": done, "n": total})
		} else if !quiet {
			unit := "MB"
			d, n := float64(done)/(1<<20), float64(total)/(1<<20)
			if step == "upgrade" {
				unit, d, n = "chunks", float64(done), float64(total)
			}
			fmt.Fprintf(os.Stderr, "\r%s: %.0f / %.0f %s   ", step, d, n, unit)
			if done == total {
				fmt.Fprint(os.Stderr, "\n")
			}
		}
	}
	dir, err := update.UpgradeWorld(in, update.UpgradeOptions{
		DataDir: dataDir, Version: java.ReadFormatName, WorkDir: work, Logf: logf, Progress: progress,
	})
	if err != nil {
		return "", err
	}
	return dir, nil
}

// removeAllRetry removes a folder; files may still be held for a moment by
// the server process that just exited.
func removeAllRetry(dir string) {
	for i := 0; i < 20; i++ {
		if err := os.RemoveAll(dir); err == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

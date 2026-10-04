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
	opts := convert.Options{
		Config:            cfg,
		Verbose:           *verbose,
		Workers:           *workers,
		CompressionLevel:  *level,
		IncludeUnfinished: *unfinished,
		LevelName:         *name,
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
	if *jsonOut {
		runJSON(in, out, from, opts)
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
	fmt.Fprintf(os.Stderr, "done in %v: %d chunks converted, %d skipped, %d failed, %d block entities, %d entities\n",
		st.Duration.Round(time.Millisecond), st.Chunks, st.Skipped, st.Failed, st.BlockEntities, st.Entities)
	if st.TranslateErrs > 0 {
		fmt.Fprintf(os.Stderr, "%d block translation warnings (see above)\n", st.TranslateErrs)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
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
		os.Exit(1)
	}
	emit(map[string]any{"t": "done", "chunks": st.Chunks, "skipped": st.Skipped, "failed": st.Failed,
		"blockEntities": st.BlockEntities, "entities": st.Entities, "ms": st.Duration.Milliseconds()})
}

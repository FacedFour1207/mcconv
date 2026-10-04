package gui

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestJobs runs real conversions through App with a built mcconv.exe:
//
//	MCCONV_EXE=../../mcconv.exe go test ./internal/gui -run TestJobs -v
func TestJobs(t *testing.T) {
	exe := os.Getenv("MCCONV_EXE")
	if exe == "" {
		t.Skip("MCCONV_EXE not set")
	}
	exe, _ = filepath.Abs(exe)
	repo := filepath.Dir(exe)

	var mu sync.Mutex
	var events []map[string]any
	exit := make(chan map[string]any, 4)
	a := newApp(func(ev map[string]any) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
		if ev["t"] == "exit" {
			exit <- ev
		}
	})
	a.exe = exe
	a.dataDir = filepath.Join(repo, "mcconv-data")
	wait := func() map[string]any {
		select {
		case ev := <-exit:
			return ev
		case <-time.After(5 * time.Minute):
			t.Fatal("timeout")
			return nil
		}
	}

	// Java -> Bedrock into a game style "minecraftWorlds" folder
	root := filepath.Join(t.TempDir(), "minecraftWorlds")
	os.MkdirAll(root, 0o755)
	out, err := a.Start(StartRequest{Source: filepath.Join(repo, "test world(JE)"), Target: "bedrock", Name: "テスト ワールド",
		DestRoot: root, Dims: []string{"overworld", "nether", "end"}, Compression: 1})
	if err != nil {
		t.Fatal(err)
	}
	if ev := wait(); ev["ok"] != true {
		t.Fatalf("conversion failed: %v", ev)
	}
	if !strings.HasSuffix(out, "=") || filepath.Dir(out) != root {
		t.Errorf("bedrock output folder %q should be a random id in %s", out, root)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "levelname.txt")); string(b) != "テスト ワールド" {
		t.Errorf("levelname.txt = %q", b)
	}
	mu.Lock()
	var gotProgress, gotDone bool
	for _, ev := range events {
		gotProgress = gotProgress || ev["t"] == "p"
		gotDone = gotDone || ev["t"] == "done"
	}
	mu.Unlock()
	if !gotProgress || !gotDone {
		t.Errorf("missing events: progress=%v done=%v", gotProgress, gotDone)
	}

	// Bedrock -> Java into a normal folder: named after the world, unique
	jroot := t.TempDir()
	os.Mkdir(filepath.Join(jroot, "My_World"), 0o755)
	out2, err := a.Start(StartRequest{Source: out, Target: "java", Name: "My/World", DestRoot: jroot, Dims: []string{"overworld"}})
	if err != nil {
		t.Fatal(err)
	}
	if ev := wait(); ev["ok"] != true {
		t.Fatalf("conversion failed: %v", ev)
	}
	if filepath.Base(out2) != "My_World (2)" {
		t.Errorf("java output folder = %q", out2)
	}
	if _, err := os.Stat(filepath.Join(out2, "level.dat")); err != nil {
		t.Error(err)
	}

	// cancel removes the partial output
	big := filepath.Join(repo, "earth_1-500")
	if _, err := os.Stat(big); err == nil {
		out3, err := a.Start(StartRequest{Source: big, Target: "bedrock", Name: "cancel", DestRoot: t.TempDir(), Dims: []string{"overworld"}})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second)
		a.Cancel()
		ev := wait()
		if ev["canceled"] != true || ev["ok"] == true {
			t.Errorf("cancel: %v", ev)
		}
		if _, err := os.Stat(out3); !os.IsNotExist(err) {
			t.Errorf("partial output %s was not removed", out3)
		}
	}

	if _, err := a.Start(StartRequest{}); err == nil {
		t.Error("empty request accepted")
	}
	w := a.Worlds()
	t.Logf("worlds: %d java, %d bedrock", len(w["java"]), len(w["bedrock"]))
}

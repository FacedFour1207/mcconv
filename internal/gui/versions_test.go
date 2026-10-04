package gui

import (
	"os"
	"testing"
)

// TestVersionLists downloads the version lists (MCCONV_NET=1 to run).
func TestVersionLists(t *testing.T) {
	if os.Getenv("MCCONV_NET") == "" {
		t.Skip("network")
	}
	var ev map[string]any
	a := newApp(func(e map[string]any) { ev = e })
	a.dataDir = t.TempDir()
	a.versionLists()
	java, _ := ev["java"].([]JavaOption)
	bedrock, _ := ev["bedrock"].([]BedrockOption)
	if ev["online"] != true || len(java) < 10 || len(bedrock) < 10 {
		t.Fatalf("online=%v java=%d bedrock=%d", ev["online"], len(java), len(bedrock))
	}
	for _, o := range java[:4] {
		t.Logf("java %+v", o)
	}
	for _, o := range bedrock[:4] {
		t.Logf("bedrock %+v", o)
	}
	if _, err := os.Stat(a.dataDir + "/version_lists.json"); err != nil {
		t.Error("cache not written")
	}
}

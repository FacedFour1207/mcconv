package update

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"mcconv/internal/translate/gen"
)

// Sources of the translation tables (all freely licensed).
const (
	geyserRaw   = "https://raw.githubusercontent.com/GeyserMC/mappings/master/"
	pmmpRaw     = "https://raw.githubusercontent.com/pmmp/BedrockData/master/"
	schemaAPI   = "https://api.github.com/repos/opencollab-incubator/BedrockBlockUpgradeSchema/contents/"
	schemaRaw   = "https://raw.githubusercontent.com/opencollab-incubator/BedrockBlockUpgradeSchema/master/"
	httpTimeout = 60 * time.Second
)

var client = &http.Client{Timeout: httpTimeout}

func download(url, dst string) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// TablesResult describes freshly built tables.
type TablesResult struct {
	JavaVersion     string
	JavaDataVersion int
	BedrockVersion  string
	Path            string
}

// UpdateTables downloads the newest GeyserMC mappings (and the pmmp /
// BedrockBlockUpgradeSchema data), reads the matching Java block states from
// the installed game with its data generator, and writes tables.json.gz.
func UpdateTables(dataDir, minecraftDir string, logf func(string, ...any)) (*TablesResult, error) {
	if minecraftDir == "" {
		minecraftDir = filepath.Join(os.Getenv("APPDATA"), ".minecraft")
	}
	tmp, err := os.MkdirTemp("", "mcconv-tables-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	geyser, pmmp, schemas := filepath.Join(tmp, "geyser"), filepath.Join(tmp, "pmmp"), filepath.Join(tmp, "schemas")
	for _, d := range []string{geyser, pmmp, schemas} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	logf("Downloading GeyserMC mappings...")
	for _, f := range []string{"README.md", "blocks.nbt", "items.json", "biomes.json"} {
		if err := download(geyserRaw+f, filepath.Join(geyser, f)); err != nil {
			return nil, err
		}
	}
	readme, _ := os.ReadFile(filepath.Join(geyser, "README.md"))
	m := regexp.MustCompile(`Java Edition ([0-9]+(?:\.[0-9]+)*) and Minecraft: Bedrock Edition ([0-9]+(?:\.[0-9]+)*)`).FindSubmatch(readme)
	if m == nil {
		return nil, fmt.Errorf("cannot read the versions from the GeyserMC mappings README")
	}
	javaVer, bedrockVer := string(m[1]), string(m[2])
	logf("  mappings for Java %s / Bedrock %s", javaVer, bedrockVer)
	for _, f := range []string{"biome_id_map.json", "banner_patterns.json", "entity_id_map.json"} {
		if err := download(pmmpRaw+f, filepath.Join(pmmp, f)); err != nil {
			return nil, err
		}
	}
	logf("Downloading Bedrock block upgrade schemas...")
	if err := downloadSchemas(schemas); err != nil {
		return nil, err
	}

	logf("Reading Java %s block states with the game's data generator...", javaVer)
	report, dv, err := runDataGenerator(minecraftDir, javaVer, filepath.Join(tmp, "gen"))
	if err != nil {
		return nil, err
	}
	data, err := gen.Build(gen.Options{Report: report, JavaVersion: javaVer, JavaDataVersion: dv,
		GeyserDir: geyser, PmmpDir: pmmp, UpgradeDir: schemas})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	out := filepath.Join(dataDir, "tables.json.gz")
	if err := os.WriteFile(out+".tmp", data, 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(out+".tmp", out); err != nil {
		return nil, err
	}
	return &TablesResult{JavaVersion: javaVer, JavaDataVersion: dv, BedrockVersion: bedrockVer, Path: out}, nil
}

func downloadSchemas(dst string) error {
	var files []string
	for _, dir := range []string{"nbt_upgrade_schema", ""} {
		resp, err := client.Get(schemaAPI + dir)
		if err != nil {
			return err
		}
		var list []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		}
		err = json.NewDecoder(resp.Body).Decode(&list)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("upgrade schema list: %w", err)
		}
		for _, e := range list {
			if e.Type == "file" && regexp.MustCompile(`^\d{4}_.*\.json$`).MatchString(e.Name) {
				p := e.Name
				if dir != "" {
					p = dir + "/" + e.Name
				}
				files = append(files, p)
			}
		}
	}
	if len(files) == 0 {
		return fmt.Errorf("no upgrade schemas found")
	}
	for _, f := range files {
		if err := download(schemaRaw+f, filepath.Join(dst, filepath.Base(f))); err != nil {
			return err
		}
	}
	return nil
}

type versionJSON struct {
	JavaVersion struct {
		Component    string `json:"component"`
		MajorVersion int    `json:"majorVersion"`
	} `json:"javaVersion"`
	Libraries []struct {
		Downloads struct {
			Artifact struct {
				Path string `json:"path"`
			} `json:"artifact"`
		} `json:"downloads"`
		Rules []struct {
			Action string `json:"action"`
			OS     struct {
				Name string `json:"name"`
			} `json:"os"`
		} `json:"rules"`
	} `json:"libraries"`
}

// runDataGenerator runs net.minecraft.data.Main --reports from an installed
// client and returns reports/blocks.json and the version's DataVersion.
func runDataGenerator(minecraftDir, version, out string) (string, int, error) {
	vdir := filepath.Join(minecraftDir, "versions", version)
	jar := filepath.Join(vdir, version+".jar")
	raw, err := os.ReadFile(filepath.Join(vdir, version+".json"))
	if err != nil {
		return "", 0, fmt.Errorf("Java Edition %s is not installed: start it once in the Minecraft launcher, then try again", version)
	}
	var vj versionJSON
	if err := json.Unmarshal(raw, &vj); err != nil {
		return "", 0, err
	}
	jv, err := readJarVersion(jar)
	if err != nil {
		return "", 0, fmt.Errorf("Java Edition %s: %v (start it once in the launcher)", version, err)
	}
	cp := []string{jar}
	for _, l := range vj.Libraries {
		if !allowedOnThisOS(l.Rules) || l.Downloads.Artifact.Path == "" {
			continue
		}
		p := filepath.Join(minecraftDir, "libraries", filepath.FromSlash(l.Downloads.Artifact.Path))
		if _, err := os.Stat(p); err != nil {
			return "", 0, fmt.Errorf("Java Edition %s is missing %s: start it once in the launcher", version, filepath.Base(p))
		}
		cp = append(cp, p)
	}
	java, err := findJavaRuntime(vj.JavaVersion.Component, vj.JavaVersion.MajorVersion)
	if err != nil {
		return "", 0, err
	}
	cmd := exec.Command(java, "-cp", strings.Join(cp, string(os.PathListSeparator)), "net.minecraft.data.Main", "--reports", "--output", out)
	hideWindow(cmd)
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Run(); err != nil {
		return "", 0, fmt.Errorf("data generator failed: %v\n%s", err, lastLines(log.String(), 8))
	}
	report := filepath.Join(out, "reports", "blocks.json")
	if _, err := os.Stat(report); err != nil {
		return "", 0, fmt.Errorf("data generator did not write %s", report)
	}
	return report, jv.WorldVersion, nil
}

func allowedOnThisOS(rules []struct {
	Action string `json:"action"`
	OS     struct {
		Name string `json:"name"`
	} `json:"os"`
}) bool {
	if len(rules) == 0 {
		return true
	}
	osName := map[string]string{"windows": "windows", "darwin": "osx", "linux": "linux"}[runtime.GOOS]
	allowed := false
	for _, r := range rules {
		if r.OS.Name == "" || r.OS.Name == osName {
			allowed = r.Action == "allow"
		}
	}
	return allowed
}

// findJavaRuntime looks for a Java runtime of at least the given major
// version: the launchers' bundled runtimes first, then JAVA_HOME and PATH.
func findJavaRuntime(component string, major int) (string, error) {
	exe := "java"
	if runtime.GOOS == "windows" {
		exe = "javaw.exe"
	}
	var cands []string
	add := func(globs ...string) {
		for _, g := range globs {
			m, _ := filepath.Glob(g)
			sort.Sort(sort.Reverse(sort.StringSlice(m)))
			cands = append(cands, m...)
		}
	}
	appdata, local, home := os.Getenv("APPDATA"), os.Getenv("LOCALAPPDATA"), os.Getenv("USERPROFILE")
	if component != "" {
		add(filepath.Join(appdata, ".minecraft", "runtime", component, "*", component, "bin", exe),
			filepath.Join(local, "Packages", "Microsoft.4297127D64EC6_8wekyb3d8bbwe", "LocalCache", "Local", "runtime", component, "*", component, "bin", exe),
			filepath.Join(os.Getenv("ProgramFiles(x86)"), "Minecraft Launcher", "runtime", component, "*", component, "bin", exe))
	}
	add(filepath.Join(appdata, ".minecraft", "runtime", "*", "*", "*", "bin", exe),
		filepath.Join(home, ".lunarclient", "jre", "*", "*", "bin", exe),
		filepath.Join(appdata, "PrismLauncher", "java", "*", "bin", exe),
		filepath.Join(os.Getenv("JAVA_HOME"), "bin", exe),
		filepath.Join(os.Getenv("ProgramFiles"), "Java", "*", "bin", exe),
		filepath.Join(os.Getenv("ProgramFiles"), "Eclipse Adoptium", "*", "bin", exe),
		filepath.Join(os.Getenv("ProgramFiles"), "Microsoft", "jdk-*", "bin", exe))
	if p, err := exec.LookPath("java"); err == nil {
		cands = append(cands, p)
	}
	for _, c := range cands {
		if javaMajor(c) >= major {
			return c, nil
		}
	}
	return "", fmt.Errorf("no Java %d runtime found: start Minecraft Java Edition once with the official launcher (it installs one) or install Java %d", major, major)
}

// javaMajor reads JAVA_VERSION from the runtime's release file.
func javaMajor(exe string) int {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(exe)), "release"))
	if err != nil {
		return 0
	}
	m := regexp.MustCompile(`JAVA_VERSION="(\d+)`).FindSubmatch(b)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(string(m[1]))
	return n
}

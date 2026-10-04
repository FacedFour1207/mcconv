package convert

import (
	"encoding/json"
	"fmt"
	"os"

	"mcconv/internal/bedrock"
	"mcconv/internal/config"
	"mcconv/internal/java"
	"mcconv/internal/translate"
	"mcconv/internal/update"
)

var setupDone bool

// setup applies the configuration (data directory) and loads the tables.
func setup(opts *Options) (*translate.Tables, error) {
	cfg := opts.Config
	if cfg == nil {
		cfg, _ = config.Load("")
		opts.Config = cfg
	}
	if !setupDone {
		setupDone = true
		if p, ok := cfg.File("tables.json.gz"); ok {
			translate.SetDataFile(p)
		}
		// entity spawn groups: from the data directory, else straight from
		// the installed Bedrock game (they are not shipped with mcconv)
		if p, ok := cfg.File("bedrock_entities.json"); ok {
			if err := loadEntityDefs(p); err != nil {
				return nil, err
			}
		} else if be, err := update.FindBedrock(""); err == nil {
			if defs, err := update.ExportBedrockEntities(be.Content); err == nil {
				for k, v := range defs {
					bedrockEntityDefs[k] = bedrockEntityDef{Spawn: v.Spawn, Born: v.Born, Health: v.Health}
				}
			}
		}
		o := cfg.Overrides
		merge(itemOverrideJ2B, o.Items.JavaToBedrock)
		merge(itemOverrideB2J, o.Items.BedrockToJava)
		merge(javaToBedrockEntityIDs, o.Entities.JavaToBedrock)
		merge(bedrockToJavaEntityIDs, o.Entities.BedrockToJava)
		v := cfg.Versions
		java.LevelDataVersion = v.JavaDataVersion
		java.LevelVersionName = v.JavaName
		bedrock.SetTargetVersion(v.BedrockVersion)
		if v.BedrockChunkVersion > 0 {
			bedrock.ChunkVersion = byte(v.BedrockChunkVersion)
		}
	}
	data, err := translate.Load()
	if err != nil {
		return nil, err
	}
	opts.Logf("block tables: %s (Java %s, Bedrock %s)", data.Source, data.JavaVersion, data.BedrockVersion)
	if len(bedrockEntityDefs) == 0 {
		opts.Logf("note: Bedrock entity definitions not found (install Minecraft Bedrock or run update-data); entities get default behaviour")
	}
	return data, nil
}

func merge(dst, src map[string]string) {
	for k, v := range src {
		dst[strip(k)] = strip(v)
	}
}

func loadEntityDefs(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	m := map[string]bedrockEntityDef{}
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for k, v := range m {
		bedrockEntityDefs[k] = v
	}
	return nil
}

// blockOverrides returns the overrides.json block mapping for a direction.
func blockOverrides(cfg *config.Config, toBedrock bool) map[string]string {
	m := cfg.Overrides.Blocks.BedrockToJava
	if toBedrock {
		m = cfg.Overrides.Blocks.JavaToBedrock
	}
	out := map[string]string{}
	for k, v := range m {
		out[withNS(k)] = withNS(v)
	}
	return out
}

func withNS(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return s
		}
	}
	return "minecraft:" + s
}

// warnNewer tells the user when the source world is newer than the tables.
func warnNewer(opts Options, what string) {
	opts.Logf("note: this world was saved by %s, newer than the block tables. Blocks unknown to the tables are copied by name;", what)
	opts.Logf("      run `mcconv update-data` to download the latest tables.")
}

// reportUnknown lists blocks that were copied by name.
func reportUnknown(opts Options, bc *blockConverter) {
	names := bc.unknownBlocks()
	if len(names) == 0 {
		return
	}
	if len(names) > 20 {
		names = append(names[:20], fmt.Sprintf("... and %d more", len(names)-20))
	}
	what := "copied by name"
	if !bc.toBedrock {
		what = "replaced with air"
	}
	opts.Logf("%d block types are not in the block tables and were %s:", len(bc.unknownBlocks()), what)
	for _, n := range names {
		opts.Logf("  %s", n)
	}
	opts.Logf("map them in overrides.json if the target edition uses a different id.")
}

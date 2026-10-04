// Package config holds the version numbers written to output worlds and
// user supplied overrides. Everything can be replaced without recompiling by
// placing files in the data directory (see README):
//
//	versions.json         target game versions
//	overrides.json        extra block / item / entity id mappings
//	tables.json.gz        block/item/biome tables (mcconv update-data)
//	bedrock_entities.json Bedrock entity spawn definitions
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Versions describes the game versions written to converted worlds.
type Versions struct {
	// Java level.dat: DataVersion and version name (e.g. 4903 / "26.2").
	JavaDataVersion int    `json:"java_data_version"`
	JavaName        string `json:"java_name"`
	// Java chunk DataVersion. 0 = the version of the translation tables, so
	// the game's DataFixer upgrades anything newer.
	JavaChunkDataVersion int `json:"java_chunk_data_version,omitempty"`

	// Bedrock version as [major, minor, patch, revision] (e.g. 1,26,52,3).
	BedrockVersion []int32 `json:"bedrock_version"`
	// Bedrock chunk format version (record tag 0x2C).
	BedrockChunkVersion int `json:"bedrock_chunk_version"`
	// Block state "version" field. 0 = the version of the translation tables.
	BedrockBlockVersion int `json:"bedrock_block_version,omitempty"`
}

// DefaultVersions are the versions this build was tested with.
func DefaultVersions() Versions {
	return Versions{
		JavaDataVersion:     4903,
		JavaName:            "26.2",
		BedrockVersion:      []int32{1, 26, 52, 3},
		BedrockChunkVersion: 42,
	}
}

// Overrides are user supplied id mappings that take precedence over the
// built-in tables. Block entries map a namespaced id to another namespaced id
// (properties are reset to the target's defaults).
type Overrides struct {
	Blocks   Mapping `json:"blocks"`
	Items    Mapping `json:"items"`
	Entities Mapping `json:"entities"`
}

// Mapping holds both directions.
type Mapping struct {
	JavaToBedrock map[string]string `json:"java_to_bedrock"`
	BedrockToJava map[string]string `json:"bedrock_to_java"`
}

// Config is the loaded configuration.
type Config struct {
	Dir       string // data directory ("" if none)
	Versions  Versions
	Overrides Overrides
}

// DefaultDir returns "<executable dir>/mcconv-data".
func DefaultDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "mcconv-data"
	}
	return filepath.Join(filepath.Dir(exe), "mcconv-data")
}

// File returns the path of a data file if it exists.
func (c *Config) File(name string) (string, bool) {
	if c.Dir == "" {
		return "", false
	}
	p := filepath.Join(c.Dir, name)
	if _, err := os.Stat(p); err != nil {
		return "", false
	}
	return p, true
}

// Load reads the data directory. A missing directory is not an error.
func Load(dir string) (*Config, error) {
	c := &Config{Versions: DefaultVersions()}
	if dir == "" {
		return c, nil
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return c, nil
	}
	c.Dir = dir
	if p, ok := c.File("versions.json"); ok {
		v := c.Versions
		if err := readJSON(p, &v); err != nil {
			return nil, err
		}
		if len(v.BedrockVersion) < 4 {
			return nil, fmt.Errorf("%s: bedrock_version needs 4 numbers", p)
		}
		c.Versions = v
	}
	if p, ok := c.File("overrides.json"); ok {
		if err := readJSON(p, &c.Overrides); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// WriteJSON writes v as indented JSON.
func WriteJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

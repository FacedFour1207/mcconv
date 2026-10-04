package java

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mcconv/internal/nbt"
	"mcconv/internal/world"
)

// Target version written to level.dat (set from config.Versions).
var (
	LevelDataVersion = 4903
	LevelVersionName = "26.2"
)

// AnvilVersion is the region format version stored in level.dat.
const AnvilVersion = 19133

// ReadDataVersion returns the DataVersion stored in level.dat (0 if unknown).
func (w *World) ReadDataVersion() int {
	root, err := ReadNBTFile(filepath.Join(w.Dir, "level.dat"))
	if err != nil {
		return 0
	}
	data, _ := root.Compound("Data")
	v, _ := data.Number("DataVersion")
	return int(v)
}

// World is a Java world directory.
type World struct {
	Dir string
	// New26 is true for the 26.x layout (dimensions/minecraft/<dim>/...).
	New26 bool
}

// Open inspects a world directory.
func Open(dir string) (*World, error) {
	if _, err := os.Stat(filepath.Join(dir, "level.dat")); err != nil {
		return nil, err
	}
	w := &World{Dir: dir}
	if st, err := os.Stat(filepath.Join(dir, "dimensions", "minecraft", "overworld")); err == nil && st.IsDir() {
		w.New26 = true
	}
	return w, nil
}

// Create prepares an empty world directory using the 26.x layout.
func Create(dir string) (*World, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &World{Dir: dir, New26: true}, nil
}

func dimName(d world.Dimension) string {
	switch d {
	case world.Nether:
		return "the_nether"
	case world.End:
		return "the_end"
	}
	return "overworld"
}

// DimDir returns the base directory of a dimension.
func (w *World) DimDir(d world.Dimension) string {
	if w.New26 {
		return filepath.Join(w.Dir, "dimensions", "minecraft", dimName(d))
	}
	switch d {
	case world.Nether:
		return filepath.Join(w.Dir, "DIM-1")
	case world.End:
		return filepath.Join(w.Dir, "DIM1")
	}
	return w.Dir
}

// RegionDir returns the terrain region directory of a dimension.
func (w *World) RegionDir(d world.Dimension) string { return filepath.Join(w.DimDir(d), "region") }

// EntitiesDir returns the entity region directory of a dimension.
func (w *World) EntitiesDir(d world.Dimension) string {
	return filepath.Join(w.DimDir(d), "entities")
}

// Bounds returns the default height of a dimension.
func Bounds(d world.Dimension) world.Bounds {
	if d == world.Overworld {
		return world.Bounds{MinSection: -4, MaxSection: 20}
	}
	return world.Bounds{MinSection: 0, MaxSection: 16}
}

// ReadNBTFile reads a gzip (or raw) compressed big endian NBT file.
func ReadNBTFile(path string) (nbt.Compound, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > 2 && data[0] == 0x1f && data[1] == 0x8b {
		gr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		data, err = io.ReadAll(gr)
		if err != nil {
			return nil, err
		}
	}
	c, _, err := nbt.DecodeCompound(data, nbt.BigEndian)
	return c, err
}

// WriteNBTFile writes a gzip compressed big endian NBT file.
func WriteNBTFile(path string, root nbt.Compound) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	gw.Write(nbt.Encode("", root, nbt.BigEndian))
	gw.Close()
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func boolOf(v any) bool {
	switch t := v.(type) {
	case string:
		return t == "true"
	}
	i, _ := nbt.AsInt(v)
	return i != 0
}

// ReadLevel extracts the convertible level settings.
func (w *World) ReadLevel() (*world.LevelInfo, error) {
	root, err := ReadNBTFile(filepath.Join(w.Dir, "level.dat"))
	if err != nil {
		return nil, err
	}
	data, _ := root.Compound("Data")
	if data == nil {
		data = nbt.Compound{}
	}
	info := &world.LevelInfo{GameRules: map[string]any{}, GenerateStructures: true}
	info.Name, _ = data.String("LevelName")
	if gt, ok := data.Number("GameType"); ok {
		info.GameType = int32(gt)
	}
	info.Commands = boolOf(data["allowCommands"])
	info.DayTime, _ = data.Number("DayTime")
	info.GameTime, _ = data.Number("Time")
	if info.DayTime == 0 {
		info.DayTime = info.GameTime
	}
	info.LastPlayed, _ = data.Number("LastPlayed")
	info.Hardcore = boolOf(data["hardcore"])
	if d, ok := data.Number("Difficulty"); ok {
		info.Difficulty = int32(d)
	} else {
		info.Difficulty = 2
	}
	if ds, ok := data.Compound("difficulty_settings"); ok {
		if s, ok := ds.String("difficulty"); ok {
			info.Difficulty = map[string]int32{"peaceful": 0, "easy": 1, "normal": 2, "hard": 3}[s]
		}
		info.Hardcore = boolOf(ds["hardcore"])
	}
	// spawn: 26.x compound or legacy SpawnX/Y/Z
	if sp, ok := data.Compound("spawn"); ok {
		if pos, ok := sp["pos"].([]int32); ok && len(pos) == 3 {
			info.SpawnX, info.SpawnY, info.SpawnZ = pos[0], pos[1], pos[2]
			info.SpawnYSet = true
		}
	} else {
		x, _ := data.Number("SpawnX")
		y, oky := data.Number("SpawnY")
		z, _ := data.Number("SpawnZ")
		info.SpawnX, info.SpawnY, info.SpawnZ, info.SpawnYSet = int32(x), int32(y), int32(z), oky
	}
	info.Raining = boolOf(data["raining"])
	info.Thundering = boolOf(data["thundering"])
	if v, ok := data.Number("rainTime"); ok {
		info.RainTime = int32(v)
	}
	if v, ok := data.Number("thunderTime"); ok {
		info.ThunderTime = int32(v)
	}
	// game rules
	if gr, ok := data.Compound("GameRules"); ok {
		for k, v := range gr {
			name, ok := world.LegacyJavaRuleNames[k]
			if !ok {
				continue
			}
			s, _ := v.(string)
			if s == "true" || s == "false" {
				info.GameRules[name] = s == "true"
			} else if n, err := parseInt(s); err == nil {
				info.GameRules[name] = int32(n)
			}
		}
	}
	if gr, err := ReadNBTFile(filepath.Join(w.Dir, "data", "minecraft", "game_rules.dat")); err == nil {
		if d, ok := gr.Compound("data"); ok {
			for k, v := range d {
				name := strings.TrimPrefix(k, "minecraft:")
				switch t := v.(type) {
				case int8:
					info.GameRules[name] = t != 0
				default:
					if n, ok := nbt.AsInt(v); ok {
						info.GameRules[name] = int32(n)
					}
				}
			}
		}
	}
	if wt, err := ReadNBTFile(filepath.Join(w.Dir, "data", "minecraft", "weather.dat")); err == nil {
		if d, ok := wt.Compound("data"); ok {
			info.Raining = boolOf(d["raining"])
			info.Thundering = boolOf(d["thundering"])
			if v, ok := d.Number("rain_time"); ok {
				info.RainTime = int32(v)
			}
			if v, ok := d.Number("thunder_time"); ok {
				info.ThunderTime = int32(v)
			}
		}
	}
	// world generation settings (seed, flat)
	var wgs nbt.Compound
	if g, ok := data.Compound("WorldGenSettings"); ok {
		wgs = g
	} else if g, err := ReadNBTFile(filepath.Join(w.Dir, "data", "minecraft", "world_gen_settings.dat")); err == nil {
		wgs, _ = g.Compound("data")
	}
	if wgs != nil {
		info.Seed, _ = wgs.Number("seed")
		if v, ok := wgs["generate_features"]; ok {
			info.GenerateStructures = boolOf(v)
		} else if v, ok := wgs["generate_structures"]; ok {
			info.GenerateStructures = boolOf(v)
		}
		if dims, ok := wgs.Compound("dimensions"); ok {
			if ow, ok := dims.Compound("minecraft:overworld"); ok {
				if gen, ok := ow.Compound("generator"); ok {
					if t, _ := gen.String("type"); t == "minecraft:flat" {
						info.Flat = true
						if st, ok := gen.Compound("settings"); ok {
							info.FlatBiome, _ = st.String("biome")
							if layers, ok := st.List("layers"); ok {
								for _, it := range layers.Items {
									lc, _ := it.(nbt.Compound)
									b, _ := lc.String("block")
									h, _ := lc.Number("height")
									info.FlatLayers = append(info.FlatLayers, world.FlatLayer{Block: b, Height: int(h)})
								}
							}
						}
					}
				}
			}
		}
	} else if s, ok := data.Number("RandomSeed"); ok {
		info.Seed = s
	}
	return info, nil
}

func parseInt(s string) (int64, error) {
	var n int64
	var neg bool
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	if s == "" {
		return 0, io.ErrUnexpectedEOF
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, io.ErrUnexpectedEOF
		}
		n = n*10 + int64(c-'0')
	}
	if neg {
		n = -n
	}
	return n, nil
}

func b8(v bool) int8 {
	if v {
		return 1
	}
	return 0
}

// WriteLevel writes level.dat and the 26.x data files.
func (w *World) WriteLevel(info *world.LevelInfo) error {
	diffNames := []string{"peaceful", "easy", "normal", "hard"}
	diff := "normal"
	if info.Difficulty >= 0 && int(info.Difficulty) < len(diffNames) {
		diff = diffNames[info.Difficulty]
	}
	last := info.LastPlayed
	if last == 0 {
		last = time.Now().UnixMilli()
	}
	spawnY := info.SpawnY
	data := nbt.Compound{
		"DataVersion": int32(LevelDataVersion),
		"version":     int32(AnvilVersion),
		"Version": nbt.Compound{
			"Id": int32(LevelDataVersion), "Name": LevelVersionName, "Series": "main", "Snapshot": int8(0),
		},
		"LevelName":     info.Name,
		"GameType":      info.GameType,
		"allowCommands": b8(info.Commands),
		"initialized":   int8(1),
		"LastPlayed":    last,
		"Time":          info.GameTime,
		"difficulty_settings": nbt.Compound{
			"difficulty": diff, "hardcore": b8(info.Hardcore), "locked": int8(0),
		},
		"spawn": nbt.Compound{
			"dimension": "minecraft:overworld",
			"pos":       []int32{info.SpawnX, spawnY, info.SpawnZ},
			"yaw":       float32(0),
			"pitch":     float32(0),
		},
		"DataPacks": nbt.Compound{
			"Enabled":  nbt.NewList("vanilla"),
			"Disabled": &nbt.List{Type: nbt.TagString},
		},
		"ServerBrands": nbt.NewList("vanilla"),
		"WasModded":    int8(0),
	}
	if err := WriteNBTFile(filepath.Join(w.Dir, "level.dat"), nbt.Compound{"Data": data}); err != nil {
		return err
	}
	dv := int32(LevelDataVersion)
	// game rules
	rules := nbt.Compound{}
	for k, v := range info.GameRules {
		switch t := v.(type) {
		case bool:
			rules["minecraft:"+k] = b8(t)
		case int32:
			rules["minecraft:"+k] = t
		}
	}
	if len(rules) > 0 {
		if err := WriteNBTFile(filepath.Join(w.Dir, "data", "minecraft", "game_rules.dat"), nbt.Compound{"data": rules, "DataVersion": dv}); err != nil {
			return err
		}
	}
	weather := nbt.Compound{
		"clear_weather_time": int32(0), "rain_time": info.RainTime, "raining": b8(info.Raining),
		"thunder_time": info.ThunderTime, "thundering": b8(info.Thundering),
	}
	if err := WriteNBTFile(filepath.Join(w.Dir, "data", "minecraft", "weather.dat"), nbt.Compound{"data": weather, "DataVersion": dv}); err != nil {
		return err
	}
	clocks := nbt.Compound{
		"minecraft:overworld": nbt.Compound{"total_ticks": info.DayTime},
		"minecraft:the_end":   nbt.Compound{"total_ticks": info.DayTime},
	}
	if err := WriteNBTFile(filepath.Join(w.Dir, "data", "minecraft", "world_clocks.dat"), nbt.Compound{"data": clocks, "DataVersion": dv}); err != nil {
		return err
	}
	overworldGen := nbt.Compound{
		"biome_source": nbt.Compound{"preset": "minecraft:overworld", "type": "minecraft:multi_noise"},
		"settings":     "minecraft:overworld",
		"type":         "minecraft:noise",
	}
	if info.Flat {
		layers := &nbt.List{Type: nbt.TagCompound}
		for _, l := range info.FlatLayers {
			layers.Items = append(layers.Items, nbt.Compound{"block": l.Block, "height": int32(l.Height)})
		}
		biome := info.FlatBiome
		if biome == "" {
			biome = "minecraft:plains"
		}
		overworldGen = nbt.Compound{
			"type": "minecraft:flat",
			"settings": nbt.Compound{
				"biome": biome, "features": int8(0), "lakes": int8(0), "layers": layers,
				"structure_overrides": &nbt.List{Type: nbt.TagString},
			},
		}
	}
	wgs := nbt.Compound{
		"bonus_chest":         int8(0),
		"generate_structures": b8(info.GenerateStructures),
		"seed":                info.Seed,
		"dimensions": nbt.Compound{
			"minecraft:overworld": nbt.Compound{"generator": overworldGen, "type": "minecraft:overworld"},
			"minecraft:the_nether": nbt.Compound{"type": "minecraft:the_nether", "generator": nbt.Compound{
				"biome_source": nbt.Compound{"preset": "minecraft:nether", "type": "minecraft:multi_noise"},
				"settings":     "minecraft:nether", "type": "minecraft:noise"}},
			"minecraft:the_end": nbt.Compound{"type": "minecraft:the_end", "generator": nbt.Compound{
				"biome_source": nbt.Compound{"type": "minecraft:the_end"},
				"settings":     "minecraft:end", "type": "minecraft:noise"}},
		},
	}
	return WriteNBTFile(filepath.Join(w.Dir, "data", "minecraft", "world_gen_settings.dat"), nbt.Compound{"data": wgs, "DataVersion": dv})
}

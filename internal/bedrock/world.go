package bedrock

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"mcconv/internal/leveldb"
	"mcconv/internal/nbt"
	"mcconv/internal/world"
)

// World is an open Bedrock world (read side).
type World struct {
	Dir string
	DB  *leveldb.DB
}

// Open opens a world directory read-only.
func Open(dir string) (*World, error) {
	db, err := leveldb.Open(filepath.Join(dir, "db"))
	if err != nil {
		return nil, err
	}
	return &World{Dir: dir, DB: db}, nil
}

// Close releases the database.
func (w *World) Close() { w.DB.Close() }

// ReadLevelDat reads level.dat (8 byte header + little endian NBT).
func ReadLevelDat(path string) (nbt.Compound, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 8 {
		return nil, fmt.Errorf("bedrock: level.dat too short")
	}
	c, _, err := nbt.DecodeCompound(data[8:], nbt.LittleEndian)
	return c, err
}

// WriteLevelDat writes level.dat with its header.
func WriteLevelDat(path string, root nbt.Compound, storageVersion int32) error {
	body := nbt.Encode("", root, nbt.LittleEndian)
	hdr := make([]byte, 8, 8+len(body))
	binary.LittleEndian.PutUint32(hdr, uint32(storageVersion))
	binary.LittleEndian.PutUint32(hdr[4:], uint32(len(body)))
	return os.WriteFile(path, append(hdr, body...), 0o644)
}

// ReadLevel extracts the convertible settings. Flat layer blocks are returned
// as Bedrock block ids.
func (w *World) ReadLevel() (*world.LevelInfo, error) {
	root, err := ReadLevelDat(filepath.Join(w.Dir, "level.dat"))
	if err != nil {
		return nil, err
	}
	num := func(k string) int64 { v, _ := root.Number(k); return v }
	b := func(k string) bool { return num(k) != 0 }
	info := &world.LevelInfo{GameRules: map[string]any{}, GenerateStructures: true}
	info.Name, _ = root.String("LevelName")
	info.Seed = num("RandomSeed")
	info.SpawnX, info.SpawnY, info.SpawnZ = int32(num("SpawnX")), int32(num("SpawnY")), int32(num("SpawnZ"))
	info.SpawnYSet = info.SpawnY != 32767
	info.GameType = int32(num("GameType"))
	info.Difficulty = int32(num("Difficulty"))
	info.Hardcore = b("IsHardcore")
	info.Commands = b("commandsEnabled") || b("cheatsEnabled")
	info.DayTime = num("Time")
	info.GameTime = num("currentTick")
	info.LastPlayed = num("LastPlayed") * 1000
	if f, ok := nbt.AsFloat(root["rainLevel"]); ok {
		info.Raining = f > 0
	}
	if f, ok := nbt.AsFloat(root["lightningLevel"]); ok {
		info.Thundering = f > 0
	}
	info.RainTime = int32(num("rainTime"))
	info.ThunderTime = int32(num("lightningTime"))
	for javaName, bedName := range world.GameRuleNames {
		v, ok := root[bedName]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case int8:
			info.GameRules[javaName] = t != 0
		case int32:
			info.GameRules[javaName] = t
		}
	}
	if num("Generator") == 2 {
		info.Flat = true
		if s, ok := root.String("FlatWorldLayers"); ok {
			var fl struct {
				BiomeID     int `json:"biome_id"`
				BlockLayers []struct {
					BlockName string `json:"block_name"`
					Count     int    `json:"count"`
				} `json:"block_layers"`
			}
			if json.Unmarshal([]byte(s), &fl) == nil {
				for _, l := range fl.BlockLayers {
					info.FlatLayers = append(info.FlatLayers, world.FlatLayer{Block: l.BlockName, Height: l.Count})
				}
				info.FlatBiome = fmt.Sprint(fl.BiomeID)
			}
		}
	}
	return info, nil
}

// ChunkPos identifies a chunk.
type ChunkPos struct {
	X, Z int32
	Dim  world.Dimension
}

// ForEachChunk streams every chunk of the database in key order.
func (w *World) ForEachChunk(fn func(*RawChunk) error) error {
	return w.ScanRange(nil, nil, fn)
}

// ChunkRanges splits the key space into about n ranges for parallel
// scanning. Boundaries are 8 byte chunk prefixes, so a chunk never spans two
// ranges.
func (w *World) ChunkRanges(n int) [][2][]byte {
	splits := w.DB.SplitKeys(n, 8)
	bounds := append([][]byte{nil}, splits...)
	bounds = append(bounds, nil)
	out := make([][2][]byte, 0, len(bounds)-1)
	for i := 0; i+1 < len(bounds); i++ {
		out = append(out, [2][]byte{bounds[i], bounds[i+1]})
	}
	return out
}

// ScanRange calls fn for every chunk whose keys lie in [lo, hi). Record
// values are not copied; they stay valid after fn returns.
func (w *World) ScanRange(lo, hi []byte, fn func(*RawChunk) error) error {
	it := w.DB.NewRangeIterator(lo, hi)
	var prefix []byte
	group := map[ChunkPos]*RawChunk{}
	var order []ChunkPos
	flush := func() error {
		for _, p := range order {
			if err := fn(group[p]); err != nil {
				return err
			}
		}
		clear(group)
		order = order[:0]
		return nil
	}
	for it.Next() {
		k := it.Key()
		x, z, dim, tag, sub, ok := ParseChunkKey(k)
		if !ok {
			continue
		}
		if prefix == nil || !bytes.Equal(prefix, k[:8]) {
			if err := flush(); err != nil {
				return err
			}
			prefix = append(prefix[:0], k[:8]...)
		}
		p := ChunkPos{x, z, dim}
		rc := group[p]
		if rc == nil {
			rc = &RawChunk{X: x, Z: z, Dim: dim, Data: map[byte][]byte{}, Sub: map[int8][]byte{}}
			group[p] = rc
			order = append(order, p)
		}
		if tag == TagSubChunk {
			rc.Sub[sub] = it.Value()
		} else {
			rc.Data[tag] = it.Value()
		}
	}
	if err := it.Err(); err != nil {
		return err
	}
	return flush()
}

// Digests reads every "digp" record (chunk -> actor ids) at once, keyed by
// the chunk key prefix.
func (w *World) Digests() (map[string][]byte, error) {
	out := map[string][]byte{}
	it := w.DB.NewRangeIterator([]byte("digp"), []byte("digq"))
	for it.Next() {
		if v := it.Value(); len(v) > 0 {
			out[string(it.Key()[4:])] = append([]byte(nil), v...)
		}
	}
	return out, it.Err()
}

// LoadRawChunk reads one chunk (used for neighbours). A chunk's records are
// contiguous in key order, so one range scan replaces a lookup per record
// (each of which would decompress a table block).
func (w *World) LoadRawChunk(x, z int32, dim world.Dimension) (*RawChunk, bool, error) {
	lo := ChunkKey(x, z, dim)
	hi := append([]byte(nil), lo...)
	for i := len(hi) - 1; i >= 0; i-- { // smallest key above every key with this prefix
		hi[i]++
		if hi[i] != 0 {
			break
		}
		if i == 0 {
			hi = nil
		}
	}
	rc := &RawChunk{X: x, Z: z, Dim: dim, Data: map[byte][]byte{}, Sub: map[int8][]byte{}}
	found := false
	it := w.DB.NewRangeIterator(lo, hi)
	for it.Next() {
		k := it.Key()
		kx, kz, kdim, tag, sub, ok := ParseChunkKey(k)
		if !ok || kx != x || kz != z || kdim != dim {
			continue // e.g. other dimensions sharing the overworld prefix
		}
		if tag == TagSubChunk {
			rc.Sub[sub] = it.Value()
		} else {
			rc.Data[tag] = it.Value()
			found = true
		}
	}
	if err := it.Err(); err != nil {
		return nil, false, err
	}
	return rc, found, nil
}

// Writer creates a new Bedrock world. Chunk records are written through
// streams whose key ranges must be disjoint (see leveldb.DBBuilder); other
// records (actors, digp, ...) are collected and written at Finish.
type Writer struct {
	Dir string
	DB  *leveldb.DBBuilder

	mu     sync.Mutex
	extras []Record
}

// Create prepares an output world directory.
func Create(dir string, compressionLevel int) (*Writer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	db, err := leveldb.NewDBBuilder(filepath.Join(dir, "db"), compressionLevel)
	if err != nil {
		return nil, err
	}
	return &Writer{Dir: dir, DB: db}, nil
}

// ChunkStream returns a stream for chunk records in ascending key order.
func (w *Writer) ChunkStream() *leveldb.TableStream { return w.DB.NewStream(6) }

// PutExtra stores a record that is not part of a chunk key range.
func (w *Writer) PutExtra(key, value []byte) {
	w.mu.Lock()
	w.extras = append(w.extras, Record{key, value})
	w.mu.Unlock()
}

// Finish writes the extra records and the database manifest.
func (w *Writer) Finish() error {
	sort.SliceStable(w.extras, func(i, j int) bool { return bytes.Compare(w.extras[i].Key, w.extras[j].Key) < 0 })
	if len(w.extras) > 0 {
		st := w.DB.NewStream(5)
		for i, r := range w.extras {
			if i+1 < len(w.extras) && bytes.Equal(r.Key, w.extras[i+1].Key) {
				continue // keep the last value of duplicate keys
			}
			if err := st.Add(r.Key, r.Value); err != nil {
				return err
			}
		}
		if err := st.Close(); err != nil {
			return err
		}
	}
	return w.DB.Finish()
}

// Target Bedrock version written to level.dat (set with SetTargetVersion).
var (
	LastOpenedVersion = []int32{1, 26, 52, 3, 0}
	MinCompatVersion  = []int32{1, 26, 0, 0, 0}
	InventoryVersion  = "1.26.52"
)

// SetTargetVersion sets the level.dat version fields from major.minor.patch.revision.
func SetTargetVersion(v []int32) {
	if len(v) < 4 {
		return
	}
	LastOpenedVersion = []int32{v[0], v[1], v[2], v[3], 0}
	MinCompatVersion = []int32{v[0], v[1], 0, 0, 0}
	InventoryVersion = fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
}

// LastOpenedWith returns lastOpenedWithVersion of a world (nil if missing).
func LastOpenedWith(dir string) []int32 {
	root, err := ReadLevelDat(filepath.Join(dir, "level.dat"))
	if err != nil {
		return nil
	}
	l, ok := root.List("lastOpenedWithVersion")
	if !ok {
		return nil
	}
	var out []int32
	for _, it := range l.Items {
		if n, ok := it.(int32); ok {
			out = append(out, n)
		}
	}
	return out
}

// WriteLevel writes level.dat and levelname.txt.
func (w *Writer) WriteLevel(info *world.LevelInfo, flatJSON string) error {
	root := defaultLevelDat()
	root["LevelName"] = info.Name
	root["RandomSeed"] = info.Seed
	root["SpawnX"], root["SpawnZ"] = info.SpawnX, info.SpawnZ
	if info.SpawnYSet {
		root["SpawnY"] = info.SpawnY
	} else {
		root["SpawnY"] = int32(32767)
	}
	root["GameType"] = info.GameType
	root["Difficulty"] = info.Difficulty
	root["IsHardcore"] = b8(info.Hardcore)
	root["commandsEnabled"] = b8(info.Commands)
	root["cheatsEnabled"] = b8(info.Commands)
	root["Time"] = info.DayTime
	root["currentTick"] = info.GameTime
	if info.LastPlayed > 0 {
		root["LastPlayed"] = info.LastPlayed / 1000
	}
	if info.Raining {
		root["rainLevel"] = float32(1)
	}
	if info.Thundering {
		root["lightningLevel"] = float32(1)
	}
	root["rainTime"] = info.RainTime
	root["lightningTime"] = info.ThunderTime
	for javaName, v := range info.GameRules {
		bed, ok := world.GameRuleNames[javaName]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case bool:
			root[bed] = b8(t)
		case int32:
			root[bed] = t
		}
	}
	if info.Flat && flatJSON != "" {
		root["Generator"] = int32(2)
		root["FlatWorldLayers"] = flatJSON
	}
	root["bonusChestEnabled"] = int8(0)
	if err := WriteLevelDat(filepath.Join(w.Dir, "level.dat"), root, 10); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(w.Dir, "levelname.txt"), []byte(info.Name), 0o644)
}

func b8(v bool) int8 {
	if v {
		return 1
	}
	return 0
}

// FlatLayersJSON renders Bedrock's FlatWorldLayers setting.
func FlatLayersJSON(layers []world.FlatLayer, biomeID int) string {
	type layer struct {
		BlockName string `json:"block_name"`
		Count     int    `json:"count"`
	}
	v := struct {
		BiomeID         int     `json:"biome_id"`
		BlockLayers     []layer `json:"block_layers"`
		EncodingVersion int     `json:"encoding_version"`
		PresetID        string  `json:"preset_id"`
		WorldVersion    string  `json:"world_version"`
	}{BiomeID: biomeID, EncodingVersion: 6, PresetID: "ClassicFlat", WorldVersion: "version.post_1_18"}
	for _, l := range layers {
		v.BlockLayers = append(v.BlockLayers, layer{l.Block, l.Height})
	}
	b, _ := json.Marshal(v)
	return string(b) + "\n"
}

func defaultLevelDat() nbt.Compound {
	return nbt.Compound{
		"abilities": nbt.Compound{
			"attackmobs": int8(1), "attackplayers": int8(1), "build": int8(1), "doorsandswitches": int8(1),
			"flying": int8(0), "flySpeed": float32(0.05), "instabuild": int8(0), "invulnerable": int8(0),
			"lightning": int8(0), "mayfly": int8(0), "mine": int8(1), "op": int8(0), "opencontainers": int8(1),
			"teleport": int8(0), "verticalFlySpeed": float32(1), "walkSpeed": float32(0.1),
		},
		"baseGameVersion":                        "*",
		"BiomeOverride":                          "",
		"bonusChestEnabled":                      int8(0),
		"bonusChestSpawned":                      int8(0),
		"CenterMapsToOrigin":                     int8(0),
		"cheatsEnabled":                          int8(0),
		"commandblockoutput":                     int8(1),
		"commandblocksenabled":                   int8(1),
		"commandsEnabled":                        int8(0),
		"ConfirmedPlatformLockedContent":         int8(0),
		"currentTick":                            int64(0),
		"daylightCycle":                          int32(0),
		"Difficulty":                             int32(2),
		"dodaylightcycle":                        int8(1),
		"doentitydrops":                          int8(1),
		"dofiretick":                             int8(1),
		"doimmediaterespawn":                     int8(0),
		"doinsomnia":                             int8(1),
		"dolimitedcrafting":                      int8(0),
		"domobloot":                              int8(1),
		"domobspawning":                          int8(1),
		"dotiledrops":                            int8(1),
		"doweathercycle":                         int8(1),
		"drowningdamage":                         int8(1),
		"editorWorldType":                        int32(0),
		"educationFeaturesEnabled":               int8(0),
		"eduOffer":                               int32(0),
		"experiments":                            nbt.Compound{"experiments_ever_used": int8(0), "saved_with_toggled_experiments": int8(0)},
		"falldamage":                             int8(1),
		"firedamage":                             int8(1),
		"ForceGameType":                          int8(0),
		"freezedamage":                           int8(1),
		"functioncommandlimit":                   int32(10000),
		"GameType":                               int32(0),
		"Generator":                              int32(1),
		"hasBeenLoadedInCreative":                int8(0),
		"hasLockedBehaviorPack":                  int8(0),
		"hasLockedResourcePack":                  int8(0),
		"immutableWorld":                         int8(0),
		"InventoryVersion":                       InventoryVersion,
		"isCreatedInEditor":                      int8(0),
		"isExportedFromEditor":                   int8(0),
		"isFromLockedTemplate":                   int8(0),
		"isFromWorldTemplate":                    int8(0),
		"IsHardcore":                             int8(0),
		"isSingleUseWorld":                       int8(0),
		"isWorldTemplateOptionLocked":            int8(0),
		"keepinventory":                          int8(0),
		"LANBroadcast":                           int8(1),
		"LANBroadcastIntent":                     int8(1),
		"lastOpenedWithVersion":                  nbt.NewList(int32s(LastOpenedVersion)...),
		"LastPlayed":                             time.Now().Unix(),
		"LevelName":                              "",
		"lightningLevel":                         float32(0),
		"lightningTime":                          int32(0),
		"limitedWorldDepth":                      int32(16),
		"limitedWorldWidth":                      int32(16),
		"LimitedWorldOriginX":                    int32(0),
		"LimitedWorldOriginY":                    int32(32767),
		"LimitedWorldOriginZ":                    int32(0),
		"maxcommandchainlength":                  int32(65535),
		"MinimumCompatibleClientVersion":         nbt.NewList(int32s(MinCompatVersion)...),
		"mobgriefing":                            int8(1),
		"MultiplayerGame":                        int8(1),
		"MultiplayerGameIntent":                  int8(1),
		"naturalregeneration":                    int8(1),
		"NetherScale":                            int32(8),
		"NetworkVersion":                         int32(0),
		"permissionsLevel":                       int32(0),
		"Platform":                               int32(2),
		"PlatformBroadcastIntent":                int32(3),
		"playerPermissionsLevel":                 int32(1),
		"playerssleepingpercentage":              int32(100),
		"prid":                                   "",
		"projectilescanbreakblocks":              int8(1),
		"pvp":                                    int8(1),
		"rainLevel":                              float32(0),
		"rainTime":                               int32(0),
		"RandomSeed":                             int64(0),
		"randomtickspeed":                        int32(1),
		"recipesunlock":                          int8(1),
		"requiresCopiedPackRemovalCheck":         int8(0),
		"respawnblocksexplode":                   int8(1),
		"sendcommandfeedback":                    int8(1),
		"serverChunkTickRange":                   int32(4),
		"showbordereffect":                       int8(1),
		"showcoordinates":                        int8(0),
		"showdaysplayed":                         int8(0),
		"showdeathmessages":                      int8(1),
		"showrecipemessages":                     int8(1),
		"showtags":                               int8(1),
		"spawnMobs":                              int8(1),
		"spawnradius":                            int32(5),
		"SpawnV1Villagers":                       int8(0),
		"SpawnX":                                 int32(0),
		"SpawnY":                                 int32(32767),
		"SpawnZ":                                 int32(0),
		"startWithMapEnabled":                    int8(0),
		"StorageVersion":                         int32(10),
		"texturePacksRequired":                   int8(0),
		"Time":                                   int64(0),
		"tntexplodes":                            int8(1),
		"tntexplosiondropdecay":                  int8(0),
		"useMsaGamertagsOnly":                    int8(0),
		"world_policies":                         nbt.Compound{},
		"worldStartCount":                        int64(0xFFFFFFFE),
		"WorldVersion":                           int32(1),
		"XBLBroadcastIntent":                     int32(3),
		"allowAnonymousBlockDropsInEditorWorlds": int8(0),
	}
}

func int32s(v []int32) []any {
	out := make([]any, len(v))
	for i, x := range v {
		out[i] = x
	}
	return out
}

// TrimNamespace removes "minecraft:" from a name.
func TrimNamespace(s string) string { return strings.TrimPrefix(s, "minecraft:") }

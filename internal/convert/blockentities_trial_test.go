package convert

import (
	"testing"

	"mcconv/internal/nbt"
	"mcconv/internal/translate"
	"mcconv/internal/world"
)

// Beehives, trial spawners and vaults keep their contents both ways.
func TestHiveTrialVault(t *testing.T) {
	data, err := translate.Load()
	if err != nil {
		t.Fatal(err)
	}
	newBE := func(toBedrock bool) *beConverter {
		items := &itemConverter{t: data, toBedrock: toBedrock}
		bc := newBlockConverter(data, toBedrock, nil, nil)
		c := &beConverter{t: data, items: items, toBedrock: toBedrock}
		c.ents = newEntityConverter(data, items, bc)
		return c
	}
	toB, toJ := newBE(true), newBE(false)
	pos := world.Pos{X: 10, Y: 64, Z: -5}
	roundTrip := func(block *translate.Block, s nbt.Compound) (bed, java nbt.Compound) {
		t.Helper()
		b := toB.toBedrockBE(block, &translate.BlockEntity{BaseName: block.BaseName, NBT: s}, pos)
		if b == nil {
			t.Fatalf("%s: no Bedrock block entity", block.BaseName)
		}
		j := toJ.toJavaBE(block, b)
		if j == nil {
			t.Fatalf("%s: no Java block entity", block.BaseName)
		}
		return b.NBT, j.NBT
	}

	// bees: the time left in the hive is kept
	hive := translate.NewBlock("minecraft:beehive", map[string]any{"facing": "north", "honey_level": "0"})
	bed, java := roundTrip(hive, nbt.Compound{"bees": &nbt.List{Type: nbt.TagCompound, Items: []any{
		nbt.Compound{"entity_data": nbt.Compound{"id": "minecraft:bee"}, "ticks_in_hive": int32(100), "min_ticks_in_hive": int32(600)},
	}}})
	occ, _ := bed.List("Occupants")
	if occ == nil || len(occ.Items) != 1 || bed["ShouldSpawnBees"] != int8(0) {
		t.Fatalf("Bedrock hive: %v", nbt.ToSNBT(bed))
	}
	o := occ.Items[0].(nbt.Compound)
	sd, _ := o.Compound("SaveData")
	if o["ActorIdentifier"] != "minecraft:bee<>" || o["TicksLeftToStay"] != int32(500) || sd["identifier"] != "minecraft:bee" {
		t.Fatalf("Bedrock occupant: %v", nbt.ToSNBT(o))
	}
	bees, _ := java.List("bees")
	if bees == nil || len(bees.Items) != 1 {
		t.Fatalf("Java hive: %v", nbt.ToSNBT(java))
	}
	jb := bees.Items[0].(nbt.Compound)
	ed, _ := jb.Compound("entity_data")
	if ed["id"] != "minecraft:bee" || ed["Pos"] != nil || jb["min_ticks_in_hive"] != int32(500) || jb["ticks_in_hive"] != int32(0) {
		t.Fatalf("Java bee: %v", nbt.ToSNBT(jb))
	}

	// trial spawner: configs and the mob
	ts := translate.NewBlock("minecraft:trial_spawner", map[string]any{"ominous": "false", "trial_spawner_state": "inactive"})
	bed, java = roundTrip(ts, nbt.Compound{
		"normal_config":  "minecraft:trial_chamber/breeze/normal",
		"ominous_config": "minecraft:trial_chamber/breeze/ominous",
		"spawn_data":     nbt.Compound{"entity": nbt.Compound{"id": "minecraft:breeze"}},
	})
	if sd, _ := bed.Compound("spawn_data"); bed["normal_config"] != "minecraft:trial_chamber/breeze/normal" || sd["TypeId"] != "minecraft:breeze" {
		t.Fatalf("Bedrock trial spawner: %v", nbt.ToSNBT(bed))
	}
	if sd, _ := java.Compound("spawn_data"); java["ominous_config"] != "minecraft:trial_chamber/breeze/ominous" || nbt.ToSNBT(sd) != `{entity: {id: "minecraft:breeze"}}` {
		t.Fatalf("Java trial spawner: %v", nbt.ToSNBT(java))
	}

	// vault: an ominous vault keeps its key and loot; a plain one gets the defaults
	vault := translate.NewBlock("minecraft:vault", map[string]any{"facing": "north", "ominous": "true", "vault_state": "inactive"})
	bed, java = roundTrip(vault, nbt.Compound{"config": nbt.Compound{
		"key_item":   nbt.Compound{"id": "minecraft:ominous_trial_key", "count": int32(1)},
		"loot_table": "minecraft:chests/trial_chambers/reward_ominous",
	}})
	cfg, _ := bed.Compound("config")
	key, _ := cfg.Compound("key_item")
	if key["Name"] != "minecraft:ominous_trial_key" || cfg["loot_table"] != "loot_tables/chests/trial_chambers/reward_ominous.json" {
		t.Fatalf("Bedrock vault: %v", nbt.ToSNBT(bed))
	}
	jcfg, _ := java.Compound("config")
	jkey, _ := jcfg.Compound("key_item")
	if jkey["id"] != "minecraft:ominous_trial_key" || jcfg["loot_table"] != "minecraft:chests/trial_chambers/reward_ominous" {
		t.Fatalf("Java vault: %v", nbt.ToSNBT(java))
	}
	bed, _ = roundTrip(vault, nbt.Compound{})
	cfg, _ = bed.Compound("config")
	key, _ = cfg.Compound("key_item")
	if key["Name"] != "minecraft:trial_key" || cfg["loot_table"] != "loot_tables/chests/trial_chambers/reward.json" {
		t.Fatalf("Bedrock default vault: %v", nbt.ToSNBT(bed))
	}
}

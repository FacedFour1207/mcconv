package convert

import (
	"maps"

	"mcconv/internal/nbt"
	"mcconv/internal/world"
)

// Beehives (and bee nests), trial spawners and vaults.
//
// Java                                   Bedrock
// beehive bees[{entity_data,             Beehive Occupants[{ActorIdentifier,
//   ticks_in_hive, min_ticks_in_hive}]     SaveData, TicksLeftToStay}], ShouldSpawnBees
// trial_spawner normal_config,           TrialSpawner normal_config, ominous_config,
//   ominous_config, spawn_data{entity}     spawn_data{TypeId, Weight}
// vault config{key_item, loot_table,     Vault config{key_item, loot_table,
//   activation_range, ...}                 activation_range, ...}, data{...}
//
// Only the settings are converted; the running state (mobs being spawned,
// players already rewarded) starts fresh, as the players differ anyway.

// entity fields Java leaves out of a bee stored in a hive
var hiveEntityDrop = []string{"Pos", "Motion", "Rotation", "UUID", "OnGround", "FallDistance", "Fire", "Air", "PortalCooldown", "Passengers", "leash"}

func (c *beConverter) beesToBedrock(s, o nbt.Compound, pos world.Pos) {
	// the Java world already decided whether the nest has bees
	o["ShouldSpawnBees"] = int8(0)
	occ := &nbt.List{Type: nbt.TagCompound}
	o["Occupants"] = occ
	l, ok := s.List("bees")
	if !ok {
		l, ok = s.List("Bees") // before 1.20.5
	}
	if !ok || c.ents == nil {
		return
	}
	for _, it := range l.Items {
		b, ok := it.(nbt.Compound)
		if !ok {
			continue
		}
		ed, ok := b.Compound("entity_data")
		if !ok {
			ed, ok = b.Compound("EntityData")
		}
		if !ok {
			continue
		}
		e := maps.Clone(ed)
		if _, ok := e["id"]; !ok {
			e["id"] = "minecraft:bee"
		}
		e["Pos"] = doubleList(float64(pos.X)+0.5, float64(pos.Y)+1, float64(pos.Z)+0.5)
		actors := c.ents.javaEntity(e, nil)
		if len(actors) == 0 {
			continue
		}
		a := actors[0]
		_, uid := c.ents.nextActorKey()
		a["UniqueID"] = uid
		ident, _ := a.String("identifier")
		ticks := firstNumber(b, "ticks_in_hive", "TicksInHive")
		minTicks := firstNumber(b, "min_ticks_in_hive", "MinOccupationTicks")
		occ.Items = append(occ.Items, nbt.Compound{
			"ActorIdentifier": ident + "<>",
			"SaveData":        a,
			"TicksLeftToStay": int32(max(minTicks-ticks, 0)),
		})
	}
}

func (c *beConverter) beesToJava(s, o nbt.Compound) {
	bees := &nbt.List{Type: nbt.TagCompound}
	o["bees"] = bees
	l, ok := s.List("Occupants")
	if !ok || c.ents == nil {
		return
	}
	for _, it := range l.Items {
		b, ok := it.(nbt.Compound)
		if !ok {
			continue
		}
		sd, ok := b.Compound("SaveData")
		if !ok {
			continue
		}
		e := c.ents.bedrockEntity(sd)
		if e == nil {
			continue
		}
		for _, k := range hiveEntityDrop {
			delete(e, k)
		}
		left, _ := b.Number("TicksLeftToStay")
		bees.Items = append(bees.Items, nbt.Compound{
			"entity_data":       e,
			"ticks_in_hive":     int32(0),
			"min_ticks_in_hive": int32(max(left, 0)),
		})
	}
}

func trialSpawnerToBedrock(s, o nbt.Compound) {
	// configs given by name; a config written out inline (custom maps)
	// has no Bedrock form and falls back to the game's default
	for _, k := range []string{"normal_config", "ominous_config"} {
		if v, ok := s.String(k); ok {
			o[k] = v
		}
	}
	rng := int64(14)
	if v, ok := s.Number("required_player_range"); ok {
		rng = v
	}
	o["required_player_range"] = int32(rng)
	if sd, ok := s.Compound("spawn_data"); ok {
		if ent, ok := sd.Compound("entity"); ok {
			if id, _ := ent.String("id"); id != "" {
				if b, ok := javaToBedrockEntityIDs[strip(id)]; ok {
					id = "minecraft:" + b
				}
				o["spawn_data"] = nbt.Compound{"TypeId": id, "Weight": int32(1)}
			}
		}
	}
	o["cooldown_end_at"] = int64(0)
	o["next_mob_spawns_at"] = int64(0)
	o["total_mobs_spawned"] = int32(0)
	o["current_mobs"] = &nbt.List{Type: nbt.TagCompound}
	o["registered_players"] = &nbt.List{Type: nbt.TagCompound}
}

func (c *beConverter) trialSpawnerToJava(s, o nbt.Compound) {
	for _, k := range []string{"normal_config", "ominous_config"} {
		if v, ok := s.String(k); ok && v != "" {
			o[k] = v
		}
	}
	if v, ok := s.Number("required_player_range"); ok {
		o["required_player_range"] = int32(v)
	}
	if sd, ok := s.Compound("spawn_data"); ok {
		if id, _ := sd.String("TypeId"); id != "" {
			if j, ok := bedrockToJavaEntityIDs[strip(id)]; ok {
				id = "minecraft:" + j
			}
			if c.jf.entity(id) {
				o["spawn_data"] = nbt.Compound{"entity": nbt.Compound{"id": id}}
			}
		}
	}
}

const (
	vaultKey  = "minecraft:trial_key"
	vaultLoot = "minecraft:chests/trial_chambers/reward"
)

func (c *beConverter) vaultToBedrock(s, o nbt.Compound) {
	cfg, _ := s.Compound("config")
	key, ok := cfg.Compound("key_item")
	if !ok {
		key = nbt.Compound{"id": vaultKey, "count": int32(1)}
	}
	bkey := c.items.convert(key)
	if bkey == nil {
		bkey = c.items.convert(nbt.Compound{"id": vaultKey, "count": int32(1)})
	}
	delete(bkey, "Slot")
	loot, ok := cfg.String("loot_table")
	if !ok {
		loot = vaultLoot
	}
	act, deact := 4.0, 4.5
	if v, ok := cfg["activation_range"]; ok {
		act, _ = nbt.AsFloat(v)
	}
	if v, ok := cfg["deactivation_range"]; ok {
		deact, _ = nbt.AsFloat(v)
	}
	display := ""
	if v, ok := cfg.String("override_loot_table_to_display"); ok && v != "" {
		display = lootJavaToBedrock(v)
	}
	o["config"] = nbt.Compound{
		"activation_range":               float32(act),
		"deactivation_range":             float32(deact),
		"key_item":                       bkey,
		"loot_table":                     lootJavaToBedrock(loot),
		"override_loot_table_to_display": display,
	}
	o["data"] = nbt.Compound{
		"items_to_eject":            &nbt.List{Type: nbt.TagCompound},
		"rewarded_players":          &nbt.List{Type: nbt.TagLong},
		"state_updating_resumes_at": int64(0),
	}
}

func (c *beConverter) vaultToJava(s, o nbt.Compound) {
	cfg, ok := s.Compound("config")
	if !ok {
		return
	}
	jc := nbt.Compound{}
	if key, ok := cfg.Compound("key_item"); ok {
		if jk := c.items.convert(key); jk != nil {
			delete(jk, "Slot")
			jc["key_item"] = jk
		}
	}
	if v, ok := cfg.String("loot_table"); ok && v != "" {
		jc["loot_table"] = lootBedrockToJava(v)
	}
	if v, ok := cfg.String("override_loot_table_to_display"); ok && v != "" {
		jc["override_loot_table_to_display"] = lootBedrockToJava(v)
	}
	for _, k := range []string{"activation_range", "deactivation_range"} {
		if v, ok := cfg[k]; ok {
			f, _ := nbt.AsFloat(v)
			jc[k] = f
		}
	}
	o["config"] = jc
}

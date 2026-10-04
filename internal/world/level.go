package world

// FlatLayer is one layer of a superflat world (bottom first).
type FlatLayer struct {
	Block  string // namespaced block id of the target edition
	Height int
}

// LevelInfo is the edition independent subset of level.dat that is converted.
type LevelInfo struct {
	Name       string
	Seed       int64
	SpawnX     int32
	SpawnY     int32
	SpawnZ     int32
	SpawnYSet  bool // false if the spawn height must be searched
	GameType   int32
	Difficulty int32
	Hardcore   bool
	Commands   bool
	DayTime    int64
	GameTime   int64
	LastPlayed int64 // unix milliseconds

	Raining     bool
	Thundering  bool
	RainTime    int32
	ThunderTime int32

	// GameRules uses Java 26 rule names without namespace (e.g. "keep_inventory").
	// Values are bool or int32.
	GameRules map[string]any

	Flat               bool
	FlatLayers         []FlatLayer // blocks are Java ids (minecraft:stone)
	FlatBiome          string      // Java biome id
	GenerateStructures bool
}

// GameRuleNames maps Java 26 game rule names to Bedrock level.dat keys.
var GameRuleNames = map[string]string{
	"advance_time":                 "dodaylightcycle",
	"advance_weather":              "doweathercycle",
	"block_drops":                  "dotiledrops",
	"command_block_output":         "commandblockoutput",
	"command_blocks_work":          "commandblocksenabled",
	"drowning_damage":              "drowningdamage",
	"entity_drops":                 "doentitydrops",
	"fall_damage":                  "falldamage",
	"fire_damage":                  "firedamage",
	"freeze_damage":                "freezedamage",
	"immediate_respawn":            "doimmediaterespawn",
	"keep_inventory":               "keepinventory",
	"limited_crafting":             "dolimitedcrafting",
	"mob_drops":                    "domobloot",
	"mob_griefing":                 "mobgriefing",
	"natural_health_regeneration":  "naturalregeneration",
	"players_sleeping_percentage":  "playerssleepingpercentage",
	"projectiles_can_break_blocks": "projectilescanbreakblocks",
	"pvp":                          "pvp",
	"random_tick_speed":            "randomtickspeed",
	"respawn_radius":               "spawnradius",
	"send_command_feedback":        "sendcommandfeedback",
	"show_death_messages":          "showdeathmessages",
	"spawn_mobs":                   "domobspawning",
	"tnt_explodes":                 "tntexplodes",
	"tnt_explosion_drop_decay":     "tntexplosiondropdecay",
	"max_command_sequence_length":  "maxcommandchainlength",
}

// LegacyJavaRuleNames maps pre-26 camelCase Java rule names to Java 26 names.
var LegacyJavaRuleNames = map[string]string{
	"doDaylightCycle":           "advance_time",
	"doWeatherCycle":            "advance_weather",
	"doTileDrops":               "block_drops",
	"commandBlockOutput":        "command_block_output",
	"drowningDamage":            "drowning_damage",
	"doEntityDrops":             "entity_drops",
	"fallDamage":                "fall_damage",
	"fireDamage":                "fire_damage",
	"freezeDamage":              "freeze_damage",
	"doImmediateRespawn":        "immediate_respawn",
	"keepInventory":             "keep_inventory",
	"doLimitedCrafting":         "limited_crafting",
	"doMobLoot":                 "mob_drops",
	"mobGriefing":               "mob_griefing",
	"naturalRegeneration":       "natural_health_regeneration",
	"playersSleepingPercentage": "players_sleeping_percentage",
	"projectilesCanBreakBlocks": "projectiles_can_break_blocks",
	"randomTickSpeed":           "random_tick_speed",
	"spawnRadius":               "respawn_radius",
	"sendCommandFeedback":       "send_command_feedback",
	"showDeathMessages":         "show_death_messages",
	"doMobSpawning":             "spawn_mobs",
	"tntExplodes":               "tnt_explodes",
	"tntExplosionDropDecay":     "tnt_explosion_drop_decay",
	"maxCommandChainLength":     "max_command_sequence_length",
	"pvp":                       "pvp",
}

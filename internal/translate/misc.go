package translate

import (
	"strconv"
	"strings"
)

// BedrockBiomeName returns the Bedrock name ("plains") of a biome id.
func (t *Tables) BedrockBiomeName(id int) (string, bool) {
	n, ok := t.bedrockBiomeNm[id]
	return n, ok
}

// BedrockBiomeID returns the id of a Bedrock biome name.
func (t *Tables) BedrockBiomeID(name string) (int, bool) {
	id, ok := t.bedrockBiomeIDs[strings.TrimPrefix(name, "minecraft:")]
	return id, ok
}

// BiomeJavaToBedrock returns the Bedrock biome name for a Java biome id.
func (t *Tables) BiomeJavaToBedrock(java string) string {
	if !strings.Contains(java, ":") {
		java = "minecraft:" + java
	}
	if id, ok := t.javaBiomeToID[java]; ok {
		if n, ok := t.bedrockBiomeNm[id]; ok {
			return n
		}
	}
	return "plains"
}

// BiomeBedrockToJava returns the Java biome id for a Bedrock biome name.
func (t *Tables) BiomeBedrockToJava(bedrock string) string {
	if id, ok := t.bedrockBiomeIDs[strings.TrimPrefix(bedrock, "minecraft:")]; ok {
		if j, ok := t.bedrockBiomeTo[id]; ok {
			return j
		}
	}
	return "minecraft:plains"
}

// ItemJavaToBedrock returns the Bedrock id and data value of a Java item.
func (t *Tables) ItemJavaToBedrock(java string) (ItemMap, bool) {
	if !strings.Contains(java, ":") {
		java = "minecraft:" + java
	}
	it, ok := t.items[java]
	if !ok {
		if n, renamed := renamedJavaItems[java]; renamed {
			it, ok = t.items[n]
		}
	}
	return it, ok
}

// ItemBedrockToJava returns the Java item for a Bedrock item id and data.
func (t *Tables) ItemBedrockToJava(bedrock string, data int) (string, bool) {
	if !strings.Contains(bedrock, ":") {
		bedrock = "minecraft:" + bedrock
	}
	if j, ok := t.itemsBack[itemKey(bedrock, data)]; ok {
		return j, true
	}
	j, ok := t.itemsBack[itemKey(bedrock, 0)]
	return j, ok
}

// renamedJavaItems maps item ids of older Java versions to current ones.
var renamedJavaItems = map[string]string{
	"minecraft:grass":      "minecraft:short_grass",
	"minecraft:scute":      "minecraft:turtle_scute",
	"minecraft:chain":      "minecraft:iron_chain",
	"minecraft:grass_path": "minecraft:dirt_path",
}

// BannerPatternToBedrock returns the Bedrock code of a Java banner pattern
// ("minecraft:border" -> "bo").
func (t *Tables) BannerPatternToBedrock(java string) (string, bool) {
	c, ok := t.patterns[strings.TrimPrefix(java, "minecraft:")]
	return c, ok
}

// BannerPatternToJava is the inverse of BannerPatternToBedrock.
func (t *Tables) BannerPatternToJava(code string) (string, bool) {
	n, ok := t.patternBack[code]
	if !ok {
		return "", false
	}
	return "minecraft:" + n, true
}

// IsBedrockEntity reports whether a Bedrock entity identifier exists.
func (t *Tables) IsBedrockEntity(id string) bool {
	if !strings.Contains(id, ":") {
		id = "minecraft:" + id
	}
	return t.entities[id]
}

// EncodeVersion packs a Bedrock version (1.21.60.33) like block states do.
func EncodeVersion(v []int32) int32 {
	var out int32
	for i := 0; i < 4; i++ {
		var n int32
		if i < len(v) {
			n = v[i]
		}
		out = out<<8 | n&0xFF
	}
	return out
}

// VersionString formats a packed Bedrock block version.
func VersionString(v int32) string {
	return strconv.Itoa(int(v>>24&0xFF)) + "." + strconv.Itoa(int(v>>16&0xFF)) + "." + strconv.Itoa(int(v>>8&0xFF)) + "." + strconv.Itoa(int(v&0xFF))
}

package convert

import (
	"strconv"
	"strings"

	"mcconv/internal/nbt"
	"mcconv/internal/translate"
)

// Item stacks: ids come from the item tables; this converts the common
// item data too: count, damage, enchantments, potions, names, lore, dye
// colour, shulker box contents and books.

var enchantIDs = []string{
	"protection", "fire_protection", "feather_falling", "blast_protection", "projectile_protection",
	"thorns", "respiration", "depth_strider", "aqua_affinity", "sharpness", "smite", "bane_of_arthropods",
	"knockback", "fire_aspect", "looting", "efficiency", "silk_touch", "unbreaking", "fortune", "power",
	"punch", "flame", "infinity", "luck_of_the_sea", "lure", "frost_walker", "mending", "binding_curse",
	"vanishing_curse", "impaling", "riptide", "loyalty", "channeling", "multishot", "piercing",
	"quick_charge", "soul_speed", "swift_sneak", "wind_burst", "density", "breach",
}

var potionIDs = []string{
	"water", "mundane", "long_mundane", "thick", "awkward", "night_vision", "long_night_vision",
	"invisibility", "long_invisibility", "leaping", "long_leaping", "strong_leaping", "fire_resistance",
	"long_fire_resistance", "swiftness", "long_swiftness", "strong_swiftness", "slowness", "long_slowness",
	"water_breathing", "long_water_breathing", "healing", "strong_healing", "harming", "strong_harming",
	"poison", "long_poison", "strong_poison", "regeneration", "long_regeneration", "strong_regeneration",
	"strength", "long_strength", "strong_strength", "weakness", "long_weakness", "decay", "turtle_master",
	"long_turtle_master", "strong_turtle_master", "slow_falling", "long_slow_falling", "strong_slowness",
	"wind_charged", "weaving", "oozing", "infested",
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

func itoa(i int) string { return strconv.Itoa(i) }

func strip(id string) string { return strings.TrimPrefix(id, "minecraft:") }

// item id overrides from overrides.json (ids without namespace)
var (
	itemOverrideJ2B = map[string]string{}
	itemOverrideB2J = map[string]string{}
)

// itemConverter converts item stacks.
type itemConverter struct {
	t         *translate.Tables
	toBedrock bool
}

// bedrockBlockFor returns the Bedrock block a Java block item places, or nil
// if the item is not a block or the Bedrock item id differs from the block.
func (ic *itemConverter) bedrockBlockFor(javaID, bedrockID string) *translate.Block {
	j, ok := ic.t.JavaDefault("minecraft:" + javaID)
	if !ok {
		return nil
	}
	b, ok := ic.t.JavaToBedrock(j)
	if !ok || b.Name() != bedrockID {
		return nil
	}
	return b
}

// ConvertItem converts one item stack. It returns nil for empty stacks.
func (ic *itemConverter) convert(item nbt.Compound) nbt.Compound {
	if item == nil {
		return nil
	}
	if ic.toBedrock {
		if _, ok := item["Name"]; ok {
			return item // already Bedrock
		}
		return ic.javaToBedrock(item)
	}
	if _, ok := item["id"]; ok && item["Name"] == nil {
		return item // already Java
	}
	return ic.bedrockToJava(item)
}

func (ic *itemConverter) convertList(l *nbt.List) *nbt.List {
	out := &nbt.List{Type: nbt.TagCompound}
	if l == nil {
		return out
	}
	for _, it := range l.Items {
		c, ok := it.(nbt.Compound)
		if !ok {
			continue
		}
		if o := ic.convert(c); o != nil {
			out.Items = append(out.Items, o)
		}
	}
	return out
}

func textToSection(v any) string {
	if v == nil {
		return ""
	}
	return translate.JavaTextToSection(v)
}

func (ic *itemConverter) javaToBedrock(item nbt.Compound) nbt.Compound {
	id, _ := item.String("id")
	id = strip(id)
	if id == "" || id == "air" {
		return nil
	}
	count := int64(1)
	if c, ok := item.Number("count"); ok {
		count = c
	} else if c, ok := item.Number("Count"); ok {
		count = c
	}
	if count <= 0 {
		return nil
	}
	comps, _ := item.Compound("components")
	if comps == nil {
		comps = nbt.Compound{}
	}
	get := func(k string) any { return comps["minecraft:"+k] }
	name, aux := id, 0
	var block *translate.Block
	if o, ok := itemOverrideJ2B[id]; ok {
		name = o
	} else if m, ok := ic.t.ItemJavaToBedrock(id); ok {
		name, aux = strip(m.ID), m.Data
		if m.Block {
			block = ic.bedrockBlockFor(id, m.ID)
		}
	}
	switch id {
	case "potion", "splash_potion", "lingering_potion", "tipped_arrow":
		p := ""
		switch t := get("potion_contents").(type) {
		case string:
			p = t
		case nbt.Compound:
			p, _ = t.String("potion")
		}
		if i := indexOf(potionIDs, strip(p)); i >= 0 {
			aux = i
		}
		if id == "tipped_arrow" {
			aux++
		}
	}
	out := nbt.Compound{
		"Name":        "minecraft:" + name,
		"Count":       int8(min(count, 127)),
		"Damage":      int16(aux),
		"WasPickedUp": int8(0),
	}
	if s, ok := item["Slot"]; ok {
		if n, ok := nbt.AsInt(s); ok {
			out["Slot"] = int8(n)
		}
	}
	if block != nil {
		states := nbt.Compound{}
		for k, v := range block.Props {
			states[k] = v
		}
		out["Block"] = nbt.Compound{"name": block.Name(), "states": states, "version": block.Version}
	}
	tag := nbt.Compound{}
	if d, ok := nbt.AsInt(get("damage")); ok && d > 0 {
		tag["Damage"] = int32(d)
	}
	ench := &nbt.List{Type: nbt.TagCompound}
	for _, key := range []string{"enchantments", "stored_enchantments"} {
		ec, ok := get(key).(nbt.Compound)
		if !ok {
			continue
		}
		levels := ec
		if l, ok := ec.Compound("levels"); ok {
			levels = l
		}
		for _, k := range levels.SortedKeys() {
			lvl, ok := nbt.AsInt(levels[k])
			if !ok {
				continue
			}
			if i := indexOf(enchantIDs, strip(k)); i >= 0 {
				ench.Items = append(ench.Items, nbt.Compound{"id": int16(i), "lvl": int16(lvl)})
			}
		}
	}
	if len(ench.Items) > 0 {
		tag["ench"] = ench
	}
	display := nbt.Compound{}
	if v := get("custom_name"); v != nil {
		display["Name"] = textToSection(v)
	} else if v := get("item_name"); v != nil && id == "written_book" {
		display["Name"] = textToSection(v)
	}
	if l, ok := get("lore").(*nbt.List); ok && len(l.Items) > 0 {
		lore := &nbt.List{Type: nbt.TagString}
		for _, it := range l.Items {
			lore.Items = append(lore.Items, textToSection(it))
		}
		display["Lore"] = lore
	}
	if len(display) > 0 {
		tag["display"] = display
	}
	switch c := get("dyed_color").(type) {
	case int32:
		tag["customColor"] = c | -16777216
	case nbt.Compound:
		if rgb, ok := c.Int("rgb"); ok {
			tag["customColor"] = rgb | -16777216
		}
	}
	if _, ok := get("unbreakable").(nbt.Compound); ok {
		tag["Unbreakable"] = int8(1)
	}
	if rc, ok := nbt.AsInt(get("repair_cost")); ok && rc > 0 {
		tag["RepairCost"] = int32(rc)
	}
	if l, ok := get("container").(*nbt.List); ok {
		items := &nbt.List{Type: nbt.TagCompound}
		for _, it := range l.Items {
			e, _ := it.(nbt.Compound)
			slot, _ := e.Number("slot")
			inner, _ := e.Compound("item")
			if conv := ic.javaToBedrock(inner); conv != nil {
				conv["Slot"] = int8(slot)
				items.Items = append(items.Items, conv)
			}
		}
		if len(items.Items) > 0 {
			tag["Items"] = items
		}
	}
	for _, key := range []string{"written_book_content", "writable_book_content"} {
		bc, ok := get(key).(nbt.Compound)
		if !ok {
			continue
		}
		pages := &nbt.List{Type: nbt.TagCompound}
		if pl, ok := bc.List("pages"); ok {
			for _, p := range pl.Items {
				var text string
				switch t := p.(type) {
				case string:
					text = t
				case nbt.Compound:
					if r, ok := t["raw"]; ok {
						if key == "written_book_content" {
							text = textToSection(r)
						} else {
							text, _ = r.(string)
						}
					} else {
						text = textToSection(t)
					}
				}
				pages.Items = append(pages.Items, nbt.Compound{"photoname": "", "text": text})
			}
		}
		tag["pages"] = pages
		if t, ok := bc.Compound("title"); ok {
			tag["title"], _ = t.String("raw")
		} else if s, ok := bc.String("title"); ok {
			tag["title"] = s
		}
		if a, ok := bc.String("author"); ok {
			tag["author"] = a
		}
		if g, ok := bc.Number("generation"); ok {
			tag["generation"] = int32(g)
		}
	}
	if len(tag) > 0 {
		out["tag"] = tag
	}
	return out
}

func (ic *itemConverter) bedrockToJava(item nbt.Compound) nbt.Compound {
	name, _ := item.String("Name")
	name = strip(name)
	if name == "" || name == "air" {
		return nil
	}
	count, _ := item.Number("Count")
	if count <= 0 {
		return nil
	}
	aux64, _ := item.Number("Damage")
	aux := int(aux64)
	tag, _ := item.Compound("tag")
	if tag == nil {
		tag = nbt.Compound{}
	}
	comps := nbt.Compound{}
	id := name
	if o, ok := itemOverrideB2J[name]; ok {
		id = o
	} else if j, ok := ic.t.ItemBedrockToJava(name, aux); ok {
		id = strip(j)
	}
	switch {
	case name == "potion" || name == "splash_potion" || name == "lingering_potion":
		if aux >= 0 && aux < len(potionIDs) && potionIDs[aux] != "decay" {
			comps["minecraft:potion_contents"] = nbt.Compound{"potion": "minecraft:" + potionIDs[aux]}
		}
	case name == "arrow" && aux > 0:
		id = "tipped_arrow"
		if aux-1 < len(potionIDs) && potionIDs[aux-1] != "decay" {
			comps["minecraft:potion_contents"] = nbt.Compound{"potion": "minecraft:" + potionIDs[aux-1]}
		}
	case name == "arrow":
		id = "arrow"
	}
	if d, ok := tag.Number("Damage"); ok && d > 0 {
		comps["minecraft:damage"] = int32(d)
	}
	if l, ok := tag.List("ench"); ok && len(l.Items) > 0 {
		levels := nbt.Compound{}
		for _, it := range l.Items {
			e, _ := it.(nbt.Compound)
			eid, _ := e.Number("id")
			lvl, _ := e.Number("lvl")
			if eid >= 0 && int(eid) < len(enchantIDs) {
				levels["minecraft:"+enchantIDs[eid]] = int32(lvl)
			}
		}
		key := "minecraft:enchantments"
		if id == "enchanted_book" {
			key = "minecraft:stored_enchantments"
		}
		comps[key] = levels
	}
	if disp, ok := tag.Compound("display"); ok {
		if n, ok := disp.String("Name"); ok && n != "" {
			comps["minecraft:custom_name"] = translate.SectionToJavaText(n)
		}
		if l, ok := disp.List("Lore"); ok && len(l.Items) > 0 {
			var lines []any
			allStrings := true
			for _, it := range l.Items {
				s, _ := it.(string)
				v := translate.SectionToJavaText(s)
				if _, ok := v.(string); !ok {
					allStrings = false
				}
				lines = append(lines, v)
			}
			lore := &nbt.List{Type: nbt.TagString, Items: lines}
			if !allStrings {
				lore.Type = nbt.TagCompound
				for i, v := range lines {
					if s, ok := v.(string); ok {
						lines[i] = nbt.Compound{"text": s}
					}
				}
			}
			comps["minecraft:lore"] = lore
		}
	}
	if c, ok := tag.Int("customColor"); ok {
		comps["minecraft:dyed_color"] = c & 0xFFFFFF
	}
	if u, _ := tag.Byte("Unbreakable"); u != 0 {
		comps["minecraft:unbreakable"] = nbt.Compound{}
	}
	if rc, ok := tag.Int("RepairCost"); ok && rc > 0 {
		comps["minecraft:repair_cost"] = rc
	}
	if l, ok := tag.List("Items"); ok && len(l.Items) > 0 {
		cont := &nbt.List{Type: nbt.TagCompound}
		for _, it := range l.Items {
			e, _ := it.(nbt.Compound)
			slot, _ := e.Number("Slot")
			if conv := ic.bedrockToJava(e); conv != nil {
				delete(conv, "Slot")
				cont.Items = append(cont.Items, nbt.Compound{"slot": int32(slot), "item": conv})
			}
		}
		comps["minecraft:container"] = cont
	}
	if pl, ok := tag.List("pages"); ok && (id == "written_book" || id == "writable_book") {
		pages := &nbt.List{Type: nbt.TagCompound}
		for _, p := range pl.Items {
			pc, _ := p.(nbt.Compound)
			text, _ := pc.String("text")
			if id == "written_book" {
				pages.Items = append(pages.Items, nbt.Compound{"raw": translate.SectionToJavaText(text)})
			} else {
				pages.Items = append(pages.Items, nbt.Compound{"raw": text})
			}
		}
		if id == "written_book" {
			title, _ := tag.String("title")
			author, _ := tag.String("author")
			gen, _ := tag.Number("generation")
			comps["minecraft:written_book_content"] = nbt.Compound{
				"title": nbt.Compound{"raw": title}, "author": author, "generation": int32(gen), "pages": pages, "resolved": int8(1),
			}
		} else {
			comps["minecraft:writable_book_content"] = nbt.Compound{"pages": pages}
		}
	}
	out := nbt.Compound{"id": "minecraft:" + id, "count": int32(count)}
	if len(comps) > 0 {
		out["components"] = comps
	}
	if s, ok := item.Number("Slot"); ok {
		out["Slot"] = int8(s)
	}
	return out
}

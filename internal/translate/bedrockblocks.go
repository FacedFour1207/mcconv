package translate

import (
	"fmt"
	"sort"

	"mcconv/internal/nbt"
)

// BedrockBlockDef is a Bedrock block and its states. Values are SNBT; the
// first value of each state is the one used when nothing else is known.
type BedrockBlockDef struct {
	Name   string         `json:"n"`
	States []BedrockState `json:"s,omitempty"`
}

// BedrockState is a Bedrock block state and its allowed values (SNBT).
type BedrockState struct {
	Name   string   `json:"n"`
	Values []string `json:"v"`
}

type bedrockDef struct {
	states []bedrockStateDef
}

type bedrockStateDef struct {
	name   string
	values []any
}

func (t *Tables) loadBedrockBlocks(defs []BedrockBlockDef) error {
	t.bedrockBlocks = make(map[string]*bedrockDef, len(defs))
	for _, d := range defs {
		bd := &bedrockDef{}
		for _, s := range d.States {
			sd := bedrockStateDef{name: s.Name}
			for _, v := range s.Values {
				pv, err := nbt.ParseSNBT(v)
				if err != nil {
					return fmt.Errorf("bedrock block %s state %s: %w", d.Name, s.Name, err)
				}
				sd.values = append(sd.values, pv)
			}
			if len(sd.values) > 0 {
				bd.states = append(bd.states, sd)
			}
		}
		t.bedrockBlocks[d.Name] = bd
	}
	return nil
}

// HasBedrockBlock reports whether name is a known Bedrock block.
func (t *Tables) HasBedrockBlock(name string) bool {
	if _, ok := t.bedrockBlocks[name]; ok {
		return true
	}
	_, ok := t.byBedrock[name]
	return ok
}

// JavaBlockNames returns every Java block id, sorted.
func (t *Tables) JavaBlockNames() []string {
	out := make([]string, 0, len(t.byName))
	for n := range t.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// BedrockBlockNames returns every known Bedrock block id, sorted.
func (t *Tables) BedrockBlockNames() []string {
	seen := make(map[string]bool, len(t.bedrockBlocks)+len(t.byBedrock))
	for n := range t.bedrockBlocks {
		seen[n] = true
	}
	for n := range t.byBedrock {
		seen[n] = true
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// BedrockState returns a state of the Bedrock block name. States of from
// that the block has (with an allowed value) are kept; the others take
// their first value. ok is false for unknown blocks.
func (t *Tables) BedrockState(name string, from map[string]any) (*Block, bool) {
	props := map[string]any{}
	if bd, ok := t.bedrockBlocks[name]; ok {
		for _, s := range bd.states {
			props[s.name] = s.values[0]
			if v, ok := from[s.name]; ok {
				for _, allowed := range s.values {
					if sameValue(v, allowed) {
						props[s.name] = allowed
						break
					}
				}
			}
		}
	} else if l := t.byBedrock[name]; len(l) > 0 {
		// only the states Java blocks convert to are known
		for k, v := range l[0].states {
			props[k] = v
		}
		for k := range props {
			if v, ok := from[k]; ok {
				for _, c := range l {
					if sameValue(v, c.states[k]) {
						props[k] = c.states[k]
						break
					}
				}
			}
		}
	} else {
		return nil, false
	}
	b := NewBlock(name, props)
	b.Version = t.BedrockBlockVersion
	return b, true
}

// UpgradeBedrock returns the current form of a Bedrock block saved by an
// older game version (b itself if it is current).
func (t *Tables) UpgradeBedrock(b *Block) *Block {
	if t.up == nil || !(b.Version == 0 || b.Version <= t.BedrockBlockVersion) {
		return b
	}
	if n, p, changed := t.up.upgrade(b.Name(), b.Props, b.Version); changed {
		nb := NewBlock(n, p)
		nb.Version = t.BedrockBlockVersion
		nb.Extra = b.Extra
		return nb
	}
	return b
}

// JavaCurrentName returns the current id of a renamed Java block.
func (t *Tables) JavaCurrentName(name string) string {
	if n, ok := renamedJavaBlocks[name]; ok {
		return n
	}
	return name
}

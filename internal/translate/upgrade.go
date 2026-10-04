package translate

import (
	"encoding/json"
	"fmt"
)

// The upgrader brings block states saved by older Bedrock versions up to
// date with BedrockBlockUpgradeSchema files (CC0). See the schema project's
// README for the rules implemented here.

type schemaValue struct {
	Int    *int32  `json:"int"`
	String *string `json:"string"`
	Byte   *int8   `json:"byte"`
}

func (v schemaValue) value() any {
	switch {
	case v.Int != nil:
		return *v.Int
	case v.Byte != nil:
		return *v.Byte
	case v.String != nil:
		return *v.String
	}
	return nil
}

type flattenRule struct {
	Prefix             string            `json:"prefix"`
	FlattenedProperty  string            `json:"flattenedProperty"`
	Suffix             string            `json:"suffix"`
	FlattenedValueMaps map[string]string `json:"flattenedValueRemaps"`
}

func (r *flattenRule) name(states map[string]any) (string, bool) {
	v, ok := states[r.FlattenedProperty]
	if !ok {
		return "", false
	}
	s := fmt.Sprint(v)
	if m, ok := r.FlattenedValueMaps[s]; ok {
		s = m
	}
	return r.Prefix + s + r.Suffix, true
}

type remapRule struct {
	OldState         map[string]schemaValue `json:"oldState"`
	NewName          string                 `json:"newName"`
	NewFlattenedName *flattenRule           `json:"newFlattenedName"`
	NewState         map[string]schemaValue `json:"newState"`
	CopiedState      []string               `json:"copiedState"`
}

type schema struct {
	Major, Minor, Patch, Revision int32
	RenamedIDs                    map[string]string                 `json:"renamedIds"`
	RenamedProperties             map[string]map[string]string      `json:"renamedProperties"`
	AddedProperties               map[string]map[string]schemaValue `json:"addedProperties"`
	RemovedProperties             map[string][]string               `json:"removedProperties"`
	RemappedPropertyValues        map[string]map[string]string      `json:"remappedPropertyValues"`
	RemappedPropertyValuesIndex   map[string][]struct {
		Old schemaValue `json:"old"`
		New schemaValue `json:"new"`
	} `json:"remappedPropertyValuesIndex"`
	FlattenedProperties map[string]flattenRule `json:"flattenedProperties"`
	RemappedStates      map[string][]remapRule `json:"remappedStates"`
	version             int32
}

type upgrader struct{ schemas []*schema }

func newUpgrader(raw []json.RawMessage) (*upgrader, error) {
	u := &upgrader{}
	for i, r := range raw {
		s := &schema{}
		if err := json.Unmarshal(r, s); err != nil {
			return nil, fmt.Errorf("upgrade schema %d: %w", i, err)
		}
		var v struct{ MaxVersionMajor, MaxVersionMinor, MaxVersionPatch, MaxVersionRevision int32 }
		json.Unmarshal(r, &v)
		s.version = v.MaxVersionMajor<<24 | v.MaxVersionMinor<<16 | v.MaxVersionPatch<<8 | v.MaxVersionRevision
		u.schemas = append(u.schemas, s)
	}
	return u, nil
}

// upgrade applies every schema newer than (or as new as: Mojang does not
// always bump the version) the state's version.
func (u *upgrader) upgrade(name string, states map[string]any, version int32) (string, map[string]any, bool) {
	changed := false
	for _, s := range u.schemas {
		if version > s.version {
			continue
		}
		if n, st, ok := s.apply(name, states); ok {
			name, states, changed = n, st, true
		}
	}
	return name, states, changed
}

func (s *schema) apply(name string, states map[string]any) (string, map[string]any, bool) {
	// remapped states replace everything else
	for _, r := range s.RemappedStates[name] {
		if !matches(states, r.OldState) {
			continue
		}
		newName := r.NewName
		if r.NewFlattenedName != nil {
			n, ok := r.NewFlattenedName.name(states)
			if !ok {
				continue
			}
			newName = n
		}
		out := map[string]any{}
		for k, v := range r.NewState {
			out[k] = v.value()
		}
		for _, k := range r.CopiedState {
			if v, ok := states[k]; ok {
				out[k] = v
			}
		}
		return newName, out, true
	}
	newName := name
	changed := false
	if n, ok := s.RenamedIDs[name]; ok {
		newName, changed = n, true
	}
	out := states
	cloned := false
	clone := func() { // copy before the first change (states is shared)
		if !cloned {
			c := make(map[string]any, len(states)+2)
			for k, v := range states {
				c[k] = v
			}
			out, cloned = c, true
		}
		changed = true
	}
	if r, ok := s.FlattenedProperties[name]; ok {
		if n, ok := r.name(states); ok {
			clone()
			newName = n
			delete(out, r.FlattenedProperty)
		}
	}
	for k, v := range s.AddedProperties[name] {
		if _, ok := states[k]; !ok {
			clone()
			out[k] = v.value()
		}
	}
	for _, k := range s.RemovedProperties[name] {
		if _, ok := states[k]; ok {
			clone()
			delete(out, k)
		}
	}
	for old, idx := range s.RemappedPropertyValues[name] {
		v, ok := states[old]
		if !ok {
			continue
		}
		for _, m := range s.RemappedPropertyValuesIndex[idx] {
			if sameValue(v, m.Old.value()) {
				clone()
				out[old] = m.New.value()
				break
			}
		}
	}
	for old, nw := range s.RenamedProperties[name] {
		if v, ok := out[old]; ok {
			clone()
			delete(out, old)
			out[nw] = v
		}
	}
	return newName, out, changed
}

func matches(states map[string]any, filter map[string]schemaValue) bool {
	for k, v := range filter {
		sv, ok := states[k]
		if !ok || !sameValue(sv, v.value()) {
			return false
		}
	}
	return true
}

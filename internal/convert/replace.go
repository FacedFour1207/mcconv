package convert

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"mcconv/internal/translate"
)

// Block replacement: the user maps source block ids to other blocks of the
// target edition (Options.Replace, and overrides.json). Properties the
// target block has (facing, half, waterlogged, ...) are kept; the others
// take their default values. Block entities of replaced blocks are dropped.
//
// Ids outside the "minecraft" namespace are add-on (Bedrock) or mod (Java)
// blocks: they are not checked and are written as given, with the states
// written after them ("ns:block[key=value,...]", optional).

// ParseReplace reads "from=to,from=to" (ids with or without "minecraft:").
func ParseReplace(s string) (map[string]string, error) {
	m := map[string]string{}
	for _, part := range splitTop(s) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		from, to, ok := strings.Cut(part, "=")
		from, to = normalizeID(from), normalizeID(to)
		if !ok || from == "" || to == "" {
			return nil, fmt.Errorf("block replacement %q: write it as from=to", part)
		}
		if strings.Contains(from, "[") {
			return nil, fmt.Errorf("block replacement %q: states can only be given for the new block", part)
		}
		if err := CheckTarget(to); err != nil {
			return nil, fmt.Errorf("block replacement %q: %w", part, err)
		}
		if _, dup := m[from]; dup {
			return nil, fmt.Errorf("block replacement: %s is listed twice", from)
		}
		m[from] = to
	}
	return m, nil
}

// splitTop splits s at the commas outside [...].
func splitTop(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// normalizeID lower-cases an id and adds "minecraft:"; a state list after
// it ("[...]") is kept as written, without spaces around its parts.
func normalizeID(s string) string {
	s = strings.TrimSpace(s)
	id, state, hasState := strings.Cut(s, "[")
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" || strings.HasSuffix(id, ":") || strings.HasPrefix(id, ":") {
		return ""
	}
	id = withNS(id)
	if !hasState {
		return id
	}
	kv, err := parseState("[" + state)
	if err != nil {
		return id + "[" + state // CheckTarget reports it
	}
	parts := make([]string, len(kv))
	for i, p := range kv {
		parts[i] = p.key + "=" + p.raw
	}
	return id + "[" + strings.Join(parts, ",") + "]"
}

// IsCustom reports whether a block id is outside the "minecraft" namespace
// (a block added by an add-on or a mod).
func IsCustom(id string) bool {
	id, _, _ = strings.Cut(id, "[")
	ns, _, ok := strings.Cut(strings.TrimSpace(id), ":")
	return ok && strings.ToLower(strings.TrimSpace(ns)) != "minecraft"
}

type stateKV struct {
	key, raw string // raw: the value as written (may be quoted)
}

// splitTarget separates "ns:block[states]" into the id and the state list.
func splitTarget(to string) (id string, kv []stateKV, err error) {
	id, rest, ok := strings.Cut(to, "[")
	if !ok {
		return id, nil, nil
	}
	kv, err = parseState("[" + rest)
	return id, kv, err
}

// parseState reads "[key=value,key=value]"; "[]" is no states.
func parseState(s string) ([]stateKV, error) {
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") || strings.Count(s, "[") != 1 || strings.Count(s, "]") != 1 {
		return nil, fmt.Errorf("write the states as [key=value,key=value]")
	}
	var out []stateKV
	seen := map[string]bool{}
	for _, p := range strings.Split(s[1:len(s)-1], ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		k, v, ok := strings.Cut(p, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("state %q: write it as key=value", p)
		}
		if seen[k] {
			return nil, fmt.Errorf("state %s is given twice", k)
		}
		quoted := len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' && strings.Count(v, `"`) == 2
		if !quoted && strings.ContainsAny(v, `"=`) {
			return nil, fmt.Errorf("state %s: bad value %s", k, v)
		}
		seen[k] = true
		out = append(out, stateKV{k, v})
	}
	return out, nil
}

// CheckTarget reports a state list that cannot be used: one that is badly
// written, or one given for a vanilla block (their states are copied from
// the source block).
func CheckTarget(to string) error {
	if !strings.Contains(to, "[") {
		return nil
	}
	if !IsCustom(to) {
		return fmt.Errorf("states can only be given for add-on or mod blocks")
	}
	_, _, err := splitTarget(to)
	return err
}

// customBlock builds an add-on / mod block from "ns:block[states]".
// Bedrock state values are typed: true/false are bytes, whole numbers are
// ints, the rest (and quoted values) strings. Java values are strings.
func customBlock(to string, bedrock bool) *translate.Block {
	id, kv, _ := splitTarget(to)
	props := make(map[string]any, len(kv))
	for _, p := range kv {
		v := p.raw
		quoted := len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"'
		if quoted {
			v = v[1 : len(v)-1]
		}
		if !bedrock || quoted {
			props[p.key] = v
			continue
		}
		switch {
		case v == "true":
			props[p.key] = int8(1)
		case v == "false":
			props[p.key] = int8(0)
		default:
			if n, err := strconv.ParseInt(v, 10, 32); err == nil {
				props[p.key] = int32(n)
			} else {
				props[p.key] = v
			}
		}
	}
	return translate.NewBlock(id, props)
}

// CheckReplace reports the first replacement that names an unknown block.
func CheckReplace(t *translate.Tables, toBedrock bool, m map[string]string) error {
	src, dst := "Java", "Bedrock"
	if !toBedrock {
		src, dst = dst, src
	}
	for _, from := range sortedKeys(m) {
		badFrom, badTo := ReplaceProblem(t, toBedrock, from, m[from])
		if badFrom {
			return fmt.Errorf("block replacement: %s is not a %s Edition block", from, src)
		}
		if badTo {
			if err := CheckTarget(m[from]); err != nil {
				return fmt.Errorf("block replacement: %s: %w", m[from], err)
			}
			return fmt.Errorf("block replacement: %s is not a %s Edition block", m[from], dst)
		}
	}
	return nil
}

// noteCustom logs the replacements that name add-on / mod blocks.
func noteCustom(opts Options, toBedrock bool) {
	need := "the mod that adds it (Fabric, NeoForge, ...)"
	if toBedrock {
		need = "the add-on (behavior pack) that adds it"
	}
	for _, from := range sortedKeys(opts.Replace) {
		to := opts.Replace[from]
		if IsCustom(from) {
			opts.Logf("note: %s is not a vanilla block; blocks with this id in the source world are replaced", from)
		}
		if IsCustom(to) {
			opts.Logf("note: %s is not a vanilla block; it is written as given. The world needs %s.", to, need)
		}
	}
}

// ReplaceProblem reports whether the source id is not a block of the
// source edition and whether the target cannot be written to the target
// edition. Ids may omit "minecraft:".
func ReplaceProblem(t *translate.Tables, toBedrock bool, from, to string) (badFrom, badTo bool) {
	from, to = withNS(from), withNS(to)
	switch {
	case IsCustom(from):
	case toBedrock:
		badFrom = !t.HasJavaBlock(t.JavaCurrentName(from))
	default:
		badFrom = !t.HasBedrockBlock(from)
	}
	if IsCustom(to) {
		return badFrom, CheckTarget(to) != nil
	}
	if strings.Contains(to, "[") {
		return badFrom, true
	}
	if toBedrock {
		return badFrom, !t.HasJavaBlock(to) && !t.HasBedrockBlock(to)
	}
	if t.HasJavaBlock(to) {
		return badFrom, false
	}
	if b, ok := t.BedrockState(to, nil); ok {
		if _, ok := t.BedrockToJava(b); ok {
			return badFrom, false
		}
	}
	return badFrom, true
}

func sortedKeys(m map[string]string) []string {
	l := make([]string, 0, len(m))
	for k := range m {
		l = append(l, k)
	}
	sort.Strings(l)
	return l
}

// replacements merges overrides.json with Options.Replace (which wins).
// Old Java ids are stored under their current name.
func replacements(t *translate.Tables, opts Options, toBedrock bool) map[string]string {
	m := blockOverrides(opts.Config, toBedrock)
	for k, v := range opts.Replace {
		k = withNS(k)
		if toBedrock {
			k = t.JavaCurrentName(k)
		}
		m[k] = withNS(v)
	}
	return m
}

// replaceTarget returns the replacement of a source block, if any, and the
// id it was matched by.
func (bc *blockConverter) replaceTarget(b *translate.Block) (src, dst string, ok bool) {
	if len(bc.override) == 0 {
		return "", "", false
	}
	name := b.Name()
	if dst, ok := bc.override[name]; ok {
		return name, dst, true
	}
	// blocks saved by older game versions
	if bc.toBedrock {
		name = bc.t.JavaCurrentName(name)
	} else {
		name = bc.t.UpgradeBedrock(b).Name()
	}
	dst, ok = bc.override[name]
	return name, dst, ok
}

// replaceBlock returns the target edition block that replaces b.
func (bc *blockConverter) replaceBlock(b *translate.Block, target string) *translate.Block {
	t := bc.t
	if IsCustom(target) {
		out := customBlock(target, bc.toBedrock)
		if bc.toBedrock {
			out.Version = t.BedrockBlockVersion
		}
		return bc.intern(out)
	}
	if bc.toBedrock {
		if t.HasJavaBlock(target) { // a Java id: its Bedrock form
			j, _ := t.JavaRename(b, target)
			if out, ok := t.JavaToBedrock(j); ok {
				return out
			}
		}
		var from map[string]any
		if src, ok := t.JavaToBedrock(b); ok {
			from = src.Props
		}
		out, ok := t.BedrockState(target, from)
		if !ok { // unknown id (overrides.json): the game fills in the states
			out = translate.NewBlock(target, nil)
			out.Version = t.BedrockBlockVersion
		}
		return bc.intern(out)
	}
	src, srcOK := t.BedrockToJava(b)
	if t.HasJavaBlock(target) {
		if srcOK {
			j, _ := t.JavaRename(src, target)
			return j
		}
		j, _ := t.JavaDefault(target)
		return j
	}
	if bs, ok := t.BedrockState(target, t.UpgradeBedrock(b).Props); ok {
		bs.Extra = b.Extra
		if j, ok := t.BedrockToJava(bs); ok {
			return j
		}
	}
	return bc.intern(translate.NewBlock(target, nil))
}

func (bc *blockConverter) countReplaced(src string, n int64) {
	c, _ := bc.replaced.LoadOrStore(src, new(atomic.Int64))
	c.(*atomic.Int64).Add(n)
}

// reportReplacements logs how many blocks each replacement changed.
func reportReplacements(opts Options, bc *blockConverter, st *Stats) {
	for _, src := range sortedKeys(bc.override) {
		var n int64
		if c, ok := bc.replaced.Load(src); ok {
			n = c.(*atomic.Int64).Load()
		}
		if !askedFor(bc, opts, src) && n == 0 {
			continue // unused overrides.json entries
		}
		if st.Replaced == nil {
			st.Replaced = map[string]int64{}
		}
		st.Replaced[src] = n
		opts.Logf("replaced %s -> %s: %d blocks", src, bc.override[src], n)
	}
}

func askedFor(bc *blockConverter, opts Options, src string) bool {
	for k := range opts.Replace {
		k = withNS(k)
		if bc.toBedrock {
			k = bc.t.JavaCurrentName(k)
		}
		if k == src {
			return true
		}
	}
	return false
}

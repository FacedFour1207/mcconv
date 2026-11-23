package convert

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"mcconv/internal/translate"
)

// Block replacement: the user maps source block ids to other blocks of the
// target edition (Options.Replace, and overrides.json). Properties the
// target block has (facing, half, waterlogged, ...) are kept; the others
// take their default values. Block entities of replaced blocks are dropped.

// ParseReplace reads "from=to,from=to" (ids with or without "minecraft:").
func ParseReplace(s string) (map[string]string, error) {
	m := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		from, to, ok := strings.Cut(part, "=")
		from, to = normalizeID(from), normalizeID(to)
		if !ok || from == "" || to == "" {
			return nil, fmt.Errorf("block replacement %q: write it as from=to", part)
		}
		if _, dup := m[from]; dup {
			return nil, fmt.Errorf("block replacement: %s is listed twice", from)
		}
		m[from] = to
	}
	return m, nil
}

func normalizeID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || strings.HasSuffix(s, ":") {
		return ""
	}
	return withNS(s)
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
			return fmt.Errorf("block replacement: %s is not a %s Edition block", m[from], dst)
		}
	}
	return nil
}

// ReplaceProblem reports whether the source id is not a block of the
// source edition and whether the target cannot be written to the target
// edition. Ids may omit "minecraft:".
func ReplaceProblem(t *translate.Tables, toBedrock bool, from, to string) (badFrom, badTo bool) {
	from, to = withNS(from), withNS(to)
	if toBedrock {
		return !t.HasJavaBlock(t.JavaCurrentName(from)), !t.HasJavaBlock(to) && !t.HasBedrockBlock(to)
	}
	badFrom = !t.HasBedrockBlock(from)
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

package translate

import (
	"sort"
	"strings"

	"mcconv/internal/nbt"
)

// Block is a block state. Property values are NBT tags (string, int8 or int32
// in practice). Extra holds additional layers (e.g. water in a waterlogged block).
type Block struct {
	Namespace string
	BaseName  string
	Props     map[string]any
	Extra     []*Block
	// Version is the Bedrock block state version the block was read with
	// (0 if unknown); it selects the translation table.
	Version int32
}

// BlockEntity is a block entity without its id/x/y/z fields.
type BlockEntity struct {
	Namespace string
	BaseName  string
	NBT       nbt.Compound
}

// NewBlock parses "namespace:name" into a block with the given properties.
func NewBlock(fullName string, props map[string]any) *Block {
	ns, name, ok := strings.Cut(fullName, ":")
	if !ok {
		ns, name = "minecraft", fullName
	}
	if props == nil {
		props = map[string]any{}
	}
	return &Block{Namespace: ns, BaseName: name, Props: props}
}

// Name returns "namespace:base_name".
func (b *Block) Name() string { return b.Namespace + ":" + b.BaseName }

// Key returns a canonical string identifying the block including extra layers.
func (b *Block) Key() string {
	var sb strings.Builder
	b.writeKey(&sb)
	return sb.String()
}

func (b *Block) writeKey(sb *strings.Builder) {
	sb.WriteString(b.Namespace)
	sb.WriteByte(':')
	sb.WriteString(b.BaseName)
	if len(b.Props) > 0 {
		keys := make([]string, 0, len(b.Props))
		for k := range b.Props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sb.WriteByte('[')
		for i, k := range keys {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(k)
			sb.WriteByte('=')
			sb.WriteString(nbt.ToSNBT(b.Props[k]))
		}
		sb.WriteByte(']')
	}
	for _, e := range b.Extra {
		sb.WriteString("{+}")
		e.writeKey(sb)
	}
}

// Layers returns the block followed by its extra layers.
func (b *Block) Layers() []*Block {
	out := make([]*Block, 0, 1+len(b.Extra))
	out = append(out, &Block{Namespace: b.Namespace, BaseName: b.BaseName, Props: b.Props, Version: b.Version})
	for _, e := range b.Extra {
		out = append(out, e.Layers()...)
	}
	return out
}

// Plus returns the block with other (and its layers) added as extra layers.
func (b *Block) Plus(other *Block) *Block {
	out := &Block{Namespace: b.Namespace, BaseName: b.BaseName, Props: b.Props, Version: b.Version}
	out.Extra = append(append([]*Block(nil), b.Extra...), &Block{Namespace: other.Namespace, BaseName: other.BaseName, Props: other.Props, Version: other.Version})
	out.Extra = append(out.Extra, other.Extra...)
	return out
}

// Clone copies the block (property values are immutable scalars).
func (b *Block) Clone() *Block {
	props := make(map[string]any, len(b.Props))
	for k, v := range b.Props {
		props[k] = v
	}
	out := &Block{Namespace: b.Namespace, BaseName: b.BaseName, Props: props, Version: b.Version}
	for _, e := range b.Extra {
		out.Extra = append(out.Extra, e.Clone())
	}
	return out
}

// Clone deep copies the block entity.
func (be *BlockEntity) Clone() *BlockEntity {
	if be == nil {
		return nil
	}
	return &BlockEntity{Namespace: be.Namespace, BaseName: be.BaseName, NBT: be.NBT.Clone()}
}

// ParseKey parses the output of Key (only for simple property values).
func ParseKey(s string) *Block {
	parts := strings.Split(s, "{+}")
	var root *Block
	for _, part := range parts {
		name, props, _ := strings.Cut(part, "[")
		b := NewBlock(name, nil)
		props = strings.TrimSuffix(props, "]")
		if props != "" {
			for _, kv := range splitProps(props) {
				k, v, _ := strings.Cut(kv, "=")
				b.Props[k], _ = nbt.ParseSNBT(v)
			}
		}
		if root == nil {
			root = b
		} else {
			root.Extra = append(root.Extra, b)
		}
	}
	return root
}

func splitProps(s string) []string {
	var out []string
	inQuote := false
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			if i == 0 || s[i-1] != '\\' {
				inQuote = !inQuote
			}
		case ',':
			if !inQuote {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

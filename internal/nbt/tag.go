// Package nbt implements the Named Binary Tag format used by both Minecraft
// editions (big endian for Java, little endian for Bedrock) plus SNBT.
//
// Tags are represented with plain Go values to keep decoding cheap:
//
//	TAG_Byte       int8
//	TAG_Short      int16
//	TAG_Int        int32
//	TAG_Long       int64
//	TAG_Float      float32
//	TAG_Double     float64
//	TAG_Byte_Array []byte
//	TAG_String     string
//	TAG_List       *List
//	TAG_Compound   Compound
//	TAG_Int_Array  []int32
//	TAG_Long_Array []int64
package nbt

import (
	"fmt"
	"sort"
	"strings"
)

// Tag ids.
const (
	TagEnd       byte = 0
	TagByte      byte = 1
	TagShort     byte = 2
	TagInt       byte = 3
	TagLong      byte = 4
	TagFloat     byte = 5
	TagDouble    byte = 6
	TagByteArray byte = 7
	TagString    byte = 8
	TagList      byte = 9
	TagCompound  byte = 10
	TagIntArray  byte = 11
	TagLongArray byte = 12
)

// Compound is a TAG_Compound.
type Compound map[string]any

// List is a TAG_List. Type is the element tag id (TagEnd for an empty untyped list).
type List struct {
	Type  byte
	Items []any
}

// NewList builds a list, inferring the element type from the first item.
func NewList(items ...any) *List {
	l := &List{Items: items}
	if len(items) > 0 {
		l.Type = TypeOf(items[0])
	}
	return l
}

// Append adds an item, setting the list type if it was empty.
func (l *List) Append(v any) {
	if len(l.Items) == 0 {
		l.Type = TypeOf(v)
	}
	l.Items = append(l.Items, v)
}

// Len returns the number of items (nil-safe).
func (l *List) Len() int {
	if l == nil {
		return 0
	}
	return len(l.Items)
}

// TypeOf returns the tag id of a Go value, or TagEnd if it is not an NBT value.
func TypeOf(v any) byte {
	switch v.(type) {
	case int8:
		return TagByte
	case int16:
		return TagShort
	case int32:
		return TagInt
	case int64:
		return TagLong
	case float32:
		return TagFloat
	case float64:
		return TagDouble
	case []byte:
		return TagByteArray
	case string:
		return TagString
	case *List:
		return TagList
	case Compound:
		return TagCompound
	case []int32:
		return TagIntArray
	case []int64:
		return TagLongArray
	}
	return TagEnd
}

// Zero returns the empty value for a tag type.
func Zero(t byte) any {
	switch t {
	case TagByte:
		return int8(0)
	case TagShort:
		return int16(0)
	case TagInt:
		return int32(0)
	case TagLong:
		return int64(0)
	case TagFloat:
		return float32(0)
	case TagDouble:
		return float64(0)
	case TagByteArray:
		return []byte{}
	case TagString:
		return ""
	case TagList:
		return &List{}
	case TagCompound:
		return Compound{}
	case TagIntArray:
		return []int32{}
	case TagLongArray:
		return []int64{}
	}
	return nil
}

// Clone deep copies a tag.
func Clone(v any) any {
	switch t := v.(type) {
	case Compound:
		return t.Clone()
	case *List:
		if t == nil {
			return (*List)(nil)
		}
		items := make([]any, len(t.Items))
		for i, it := range t.Items {
			items[i] = Clone(it)
		}
		return &List{Type: t.Type, Items: items}
	case []byte:
		return append([]byte(nil), t...)
	case []int32:
		return append([]int32(nil), t...)
	case []int64:
		return append([]int64(nil), t...)
	}
	return v
}

// Clone deep copies the compound.
func (c Compound) Clone() Compound {
	if c == nil {
		return nil
	}
	out := make(Compound, len(c))
	for k, v := range c {
		out[k] = Clone(v)
	}
	return out
}

// SortedKeys returns the keys in the order SNBT output uses.
func (c Compound) SortedKeys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := strings.ToLower(keys[i]), strings.ToLower(keys[j])
		if a != b {
			return a < b
		}
		return keys[i] > keys[j]
	})
	return keys
}

// Typed getters. They return the zero value and false when missing or of another type.

func (c Compound) Compound(key string) (Compound, bool) {
	v, ok := c[key].(Compound)
	return v, ok
}

func (c Compound) List(key string) (*List, bool) {
	v, ok := c[key].(*List)
	return v, ok
}

func (c Compound) String(key string) (string, bool) {
	v, ok := c[key].(string)
	return v, ok
}

func (c Compound) Byte(key string) (int8, bool) {
	v, ok := c[key].(int8)
	return v, ok
}

func (c Compound) Short(key string) (int16, bool) {
	v, ok := c[key].(int16)
	return v, ok
}

func (c Compound) Int(key string) (int32, bool) {
	v, ok := c[key].(int32)
	return v, ok
}

func (c Compound) Long(key string) (int64, bool) {
	v, ok := c[key].(int64)
	return v, ok
}

func (c Compound) Float(key string) (float32, bool) {
	v, ok := c[key].(float32)
	return v, ok
}

func (c Compound) Double(key string) (float64, bool) {
	v, ok := c[key].(float64)
	return v, ok
}

// Number returns any numeric tag as int64.
func (c Compound) Number(key string) (int64, bool) {
	return AsInt(c[key])
}

// AsInt converts any integer or floating tag to int64.
func AsInt(v any) (int64, bool) {
	switch t := v.(type) {
	case int8:
		return int64(t), true
	case int16:
		return int64(t), true
	case int32:
		return int64(t), true
	case int64:
		return t, true
	case float32:
		return int64(t), true
	case float64:
		return int64(t), true
	}
	return 0, false
}

// AsFloat converts any numeric tag to float64.
func AsFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float32:
		return float64(t), true
	case float64:
		return t, true
	}
	i, ok := AsInt(v)
	return float64(i), ok
}

// Equal compares two tags deeply.
func Equal(a, b any) bool {
	switch x := a.(type) {
	case Compound:
		y, ok := b.(Compound)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !Equal(v, w) {
				return false
			}
		}
		return true
	case *List:
		y, ok := b.(*List)
		if !ok || len(x.Items) != len(y.Items) {
			return false
		}
		for i := range x.Items {
			if !Equal(x.Items[i], y.Items[i]) {
				return false
			}
		}
		return true
	case []byte:
		y, ok := b.([]byte)
		return ok && string(x) == string(y)
	case []int32:
		y, ok := b.([]int32)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	case []int64:
		y, ok := b.([]int64)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	}
	return a == b
}

// Errorf is a small helper for decode errors.
func errorf(format string, args ...any) error {
	return fmt.Errorf("nbt: "+format, args...)
}

package translate

import (
	"encoding/json"
	"strings"

	"mcconv/internal/nbt"
)

// Java Edition stores formatted text as text components (JSON strings in
// older worlds, NBT since 1.21.5); Bedrock Edition stores plain strings with
// "§" formatting codes. These functions convert between the two.

var colourCodes = map[string]byte{
	"black": '0', "dark_blue": '1', "dark_green": '2', "dark_aqua": '3', "dark_red": '4',
	"dark_purple": '5', "gold": '6', "gray": '7', "dark_gray": '8', "blue": '9', "green": 'a',
	"aqua": 'b', "red": 'c', "light_purple": 'd', "yellow": 'e', "white": 'f',
}

var colourNames = func() map[byte]string {
	m := map[byte]string{}
	for n, c := range colourCodes {
		m[c] = n
	}
	return m
}()

// rgb of the named colours, for mapping "#rrggbb" to the nearest one
var colourRGB = map[string][3]int{
	"black": {0, 0, 0}, "dark_blue": {0, 0, 170}, "dark_green": {0, 170, 0}, "dark_aqua": {0, 170, 170},
	"dark_red": {170, 0, 0}, "dark_purple": {170, 0, 170}, "gold": {255, 170, 0}, "gray": {170, 170, 170},
	"dark_gray": {85, 85, 85}, "blue": {85, 85, 255}, "green": {85, 255, 85}, "aqua": {85, 255, 255},
	"red": {255, 85, 85}, "light_purple": {255, 85, 255}, "yellow": {255, 255, 85}, "white": {255, 255, 255},
}

type style struct {
	colour                                      byte // 0 = none
	bold, italic, underline, strike, obfuscated bool
}

func (s style) codes() string {
	var b strings.Builder
	if s.colour != 0 {
		b.WriteString("§")
		b.WriteByte(s.colour)
	}
	for _, f := range []struct {
		on   bool
		code byte
	}{{s.obfuscated, 'k'}, {s.bold, 'l'}, {s.strike, 'm'}, {s.underline, 'n'}, {s.italic, 'o'}} {
		if f.on {
			b.WriteString("§")
			b.WriteByte(f.code)
		}
	}
	return b.String()
}

// JavaTextToSection renders a Java text component as a "§" formatted string.
func JavaTextToSection(v any) string {
	var out strings.Builder
	var cur style
	renderText(&out, &cur, style{}, normalizeText(v))
	return out.String()
}

// normalizeText decodes JSON text stored in strings (older Java versions).
func normalizeText(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	t := strings.TrimSpace(s)
	if t == "" || (t[0] != '{' && t[0] != '[' && t[0] != '"') {
		return s
	}
	var j any
	if json.Unmarshal([]byte(t), &j) != nil {
		return s
	}
	return fromJSON(j)
}

func fromJSON(j any) any {
	switch t := j.(type) {
	case map[string]any:
		c := nbt.Compound{}
		for k, v := range t {
			c[k] = fromJSON(v)
		}
		return c
	case []any:
		l := &nbt.List{}
		for _, v := range t {
			l.Items = append(l.Items, fromJSON(v))
		}
		return l
	case bool:
		if t {
			return int8(1)
		}
		return int8(0)
	case float64:
		return t
	}
	return j
}

func renderText(out *strings.Builder, cur *style, parent style, v any) {
	switch t := v.(type) {
	case string:
		emit(out, cur, parent, t)
	case *nbt.List:
		for i, it := range t.Items {
			if i == 0 {
				renderText(out, cur, parent, it)
				continue
			}
			// later list entries inherit the first entry's style
			renderText(out, cur, styleOf(t.Items[0], parent), it)
		}
	case nbt.Compound:
		st := styleOf(t, parent)
		text, _ := t.String("text")
		if text == "" {
			if tr, ok := t.String("translate"); ok {
				text = tr
				if fb, ok := t.String("fallback"); ok {
					text = fb
				}
			}
		}
		emit(out, cur, st, text)
		if ex, ok := t.List("extra"); ok {
			for _, it := range ex.Items {
				renderText(out, cur, st, it)
			}
		}
	}
}

func styleOf(v any, parent style) style {
	c, ok := v.(nbt.Compound)
	if !ok {
		return parent
	}
	st := parent
	if col, ok := c.String("color"); ok {
		st.colour = colourCode(col)
	}
	flag := func(key string, dst *bool) {
		if f, ok := c[key]; ok {
			n, _ := nbt.AsInt(f)
			if b, isStr := f.(string); isStr {
				*dst = b == "true"
			} else {
				*dst = n != 0
			}
		}
	}
	flag("bold", &st.bold)
	flag("italic", &st.italic)
	flag("underlined", &st.underline)
	flag("strikethrough", &st.strike)
	flag("obfuscated", &st.obfuscated)
	return st
}

func colourCode(name string) byte {
	if c, ok := colourCodes[name]; ok {
		return c
	}
	if len(name) == 7 && name[0] == '#' {
		var rgb [3]int
		for i := range 3 {
			for _, ch := range name[1+2*i : 3+2*i] {
				rgb[i] *= 16
				switch {
				case ch >= '0' && ch <= '9':
					rgb[i] += int(ch - '0')
				case ch >= 'a' && ch <= 'f':
					rgb[i] += int(ch-'a') + 10
				case ch >= 'A' && ch <= 'F':
					rgb[i] += int(ch-'A') + 10
				}
			}
		}
		best, bestD := "white", 1<<30
		for n, c := range colourRGB {
			d := (c[0]-rgb[0])*(c[0]-rgb[0]) + (c[1]-rgb[1])*(c[1]-rgb[1]) + (c[2]-rgb[2])*(c[2]-rgb[2])
			if d < bestD || d == bestD && n < best {
				best, bestD = n, d
			}
		}
		return colourCodes[best]
	}
	return 0
}

func emit(out *strings.Builder, cur *style, st style, text string) {
	if text == "" {
		return
	}
	if st != *cur {
		if *cur != (style{}) {
			out.WriteString("§r")
		}
		out.WriteString(st.codes())
		*cur = st
	}
	out.WriteString(text)
}

// SectionToJavaText converts a "§" formatted string to a Java text
// component: the plain string when it has no formatting, otherwise a
// compound with one child per formatted run.
func SectionToJavaText(s string) any {
	if !strings.Contains(s, "§") {
		return s
	}
	var parts []any
	var st style
	var run strings.Builder
	flush := func() {
		if run.Len() == 0 {
			return
		}
		c := nbt.Compound{"text": run.String()}
		if st.colour != 0 {
			c["color"] = colourNames[st.colour]
		}
		for _, f := range []struct {
			on  bool
			key string
		}{{st.bold, "bold"}, {st.italic, "italic"}, {st.underline, "underlined"}, {st.strike, "strikethrough"}, {st.obfuscated, "obfuscated"}} {
			if f.on {
				c[f.key] = int8(1)
			}
		}
		if len(c) == 1 {
			parts = append(parts, c["text"])
		} else {
			parts = append(parts, c)
		}
		run.Reset()
	}
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		if rs[i] != '§' || i+1 >= len(rs) {
			run.WriteRune(rs[i])
			continue
		}
		code := byte(rs[i+1])
		i++
		flush()
		switch {
		case colourNames[code] != "":
			st = style{colour: code} // a colour resets the formatting
		case code == 'k':
			st.obfuscated = true
		case code == 'l':
			st.bold = true
		case code == 'm':
			st.strike = true
		case code == 'n':
			st.underline = true
		case code == 'o':
			st.italic = true
		case code == 'r':
			st = style{}
		}
	}
	flush()
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		if s, ok := parts[0].(string); ok {
			return s
		}
		return parts[0]
	}
	extra := &nbt.List{Items: parts}
	extra.Type = nbt.TagCompound
	for i, p := range parts {
		if s, ok := p.(string); ok {
			parts[i] = nbt.Compound{"text": s}
		}
	}
	return nbt.Compound{"text": "", "extra": extra}
}

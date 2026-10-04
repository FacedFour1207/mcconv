package nbt

import (
	"math"
	"strconv"
	"strings"
)

// ToSNBT formats a tag as SNBT with sorted compound keys. The output is used
// as a lookup key, so it is canonical: equal tags give equal strings.
func ToSNBT(v any) string {
	var sb strings.Builder
	writeSNBT(&sb, v)
	return sb.String()
}

func writeSNBT(sb *strings.Builder, v any) {
	switch t := v.(type) {
	case int8:
		sb.WriteString(strconv.Itoa(int(t)))
		sb.WriteByte('b')
	case int16:
		sb.WriteString(strconv.Itoa(int(t)))
		sb.WriteByte('s')
	case int32:
		sb.WriteString(strconv.Itoa(int(t)))
	case int64:
		sb.WriteString(strconv.FormatInt(t, 10))
		sb.WriteByte('L')
	case float32:
		sb.WriteString(pyFloat(float64(t)))
		sb.WriteByte('f')
	case float64:
		sb.WriteString(pyFloat(t))
		sb.WriteByte('d')
	case string:
		writeQuoted(sb, t)
	case []byte:
		sb.WriteString("[B;")
		for i, x := range t {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(strconv.Itoa(int(int8(x))))
			sb.WriteByte('B')
		}
		sb.WriteByte(']')
	case []int32:
		sb.WriteString("[I;")
		for i, x := range t {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(strconv.Itoa(int(x)))
		}
		sb.WriteByte(']')
	case []int64:
		sb.WriteString("[L;")
		for i, x := range t {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(strconv.FormatInt(x, 10))
			sb.WriteByte('L')
		}
		sb.WriteByte(']')
	case *List:
		sb.WriteByte('[')
		if t != nil {
			for i, x := range t.Items {
				if i > 0 {
					sb.WriteString(", ")
				}
				writeSNBT(sb, x)
			}
		}
		sb.WriteByte(']')
	case Compound:
		sb.WriteByte('{')
		for i, k := range t.SortedKeys() {
			if i > 0 {
				sb.WriteString(", ")
			}
			if isBareKey(k) {
				sb.WriteString(k)
			} else {
				writeQuoted(sb, k)
			}
			sb.WriteString(": ")
			writeSNBT(sb, t[k])
		}
		sb.WriteByte('}')
	default:
		sb.WriteString("{}")
	}
}

func isBareKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c == '+') {
			return false
		}
	}
	return true
}

func writeQuoted(sb *strings.Builder, s string) {
	sb.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' || c == '\\' {
			sb.WriteByte('\\')
		}
		sb.WriteByte(c)
	}
	sb.WriteByte('"')
}

// pyFloat mimics Python's repr() of a float.
func pyFloat(f float64) string {
	if math.IsInf(f, 1) {
		return "inf"
	}
	if math.IsInf(f, -1) {
		return "-inf"
	}
	if math.IsNaN(f) {
		return "nan"
	}
	s := strconv.FormatFloat(f, 'e', -1, 64) // d.ddddde±XX
	mant, expStr, _ := strings.Cut(s, "e")
	exp, _ := strconv.Atoi(expStr)
	if exp < -4 || exp >= 16 {
		sign := "+"
		if exp < 0 {
			sign = "-"
			exp = -exp
		}
		es := strconv.Itoa(exp)
		if len(es) < 2 {
			es = "0" + es
		}
		return mant + "e" + sign + es
	}
	s = strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".") {
		s += ".0"
	}
	return s
}

// ParseSNBT parses SNBT (lenient: unquoted strings and keys are accepted).
func ParseSNBT(s string) (any, error) {
	p := snbtParser{s: s}
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	return v, nil
}

// MustSNBT parses SNBT and panics on error (for embedded data).
func MustSNBT(s string) any {
	v, err := ParseSNBT(s)
	if err != nil {
		panic(err)
	}
	return v
}

type snbtParser struct {
	s   string
	pos int
}

func (p *snbtParser) skipWS() {
	for p.pos < len(p.s) {
		switch p.s[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *snbtParser) err(msg string) error {
	return errorf("snbt: %s at %d in %q", msg, p.pos, p.s)
}

func (p *snbtParser) value() (any, error) {
	p.skipWS()
	if p.pos >= len(p.s) {
		return nil, p.err("unexpected end")
	}
	switch c := p.s[p.pos]; c {
	case '{':
		return p.compound()
	case '[':
		return p.list()
	case '"', '\'':
		return p.quoted()
	default:
		tok := p.bare()
		if tok == "" {
			return nil, p.err("expected value")
		}
		return parseScalar(tok), nil
	}
}

func (p *snbtParser) bare() string {
	start := p.pos
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if c == ',' || c == ']' || c == '}' || c == ':' || c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '[' || c == '{' {
			break
		}
		p.pos++
	}
	return p.s[start:p.pos]
}

func (p *snbtParser) quoted() (string, error) {
	q := p.s[p.pos]
	p.pos++
	var sb strings.Builder
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		p.pos++
		if c == '\\' && p.pos < len(p.s) {
			sb.WriteByte(p.s[p.pos])
			p.pos++
			continue
		}
		if c == q {
			return sb.String(), nil
		}
		sb.WriteByte(c)
	}
	return "", p.err("unterminated string")
}

func (p *snbtParser) compound() (any, error) {
	p.pos++ // {
	c := Compound{}
	for {
		p.skipWS()
		if p.pos >= len(p.s) {
			return nil, p.err("unterminated compound")
		}
		if p.s[p.pos] == '}' {
			p.pos++
			return c, nil
		}
		var key string
		if p.s[p.pos] == '"' || p.s[p.pos] == '\'' {
			k, err := p.quoted()
			if err != nil {
				return nil, err
			}
			key = k
		} else {
			key = p.bare()
		}
		p.skipWS()
		if p.pos >= len(p.s) || p.s[p.pos] != ':' {
			return nil, p.err("expected ':'")
		}
		p.pos++
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		c[key] = v
		p.skipWS()
		if p.pos < len(p.s) && p.s[p.pos] == ',' {
			p.pos++
		}
	}
}

func (p *snbtParser) list() (any, error) {
	p.pos++ // [
	p.skipWS()
	// typed arrays
	if p.pos+1 < len(p.s) && p.s[p.pos+1] == ';' {
		kind := p.s[p.pos]
		p.pos += 2
		var nums []int64
		for {
			p.skipWS()
			if p.pos >= len(p.s) {
				return nil, p.err("unterminated array")
			}
			if p.s[p.pos] == ']' {
				p.pos++
				break
			}
			tok := strings.TrimRight(p.bare(), "bBlLsS")
			n, err := strconv.ParseInt(tok, 10, 64)
			if err != nil {
				return nil, p.err("bad array element")
			}
			nums = append(nums, n)
			p.skipWS()
			if p.pos < len(p.s) && p.s[p.pos] == ',' {
				p.pos++
			}
		}
		switch kind {
		case 'B', 'b':
			out := make([]byte, len(nums))
			for i, n := range nums {
				out[i] = byte(n)
			}
			return out, nil
		case 'I', 'i':
			out := make([]int32, len(nums))
			for i, n := range nums {
				out[i] = int32(n)
			}
			return out, nil
		default:
			return nums, nil
		}
	}
	l := &List{}
	for {
		p.skipWS()
		if p.pos >= len(p.s) {
			return nil, p.err("unterminated list")
		}
		if p.s[p.pos] == ']' {
			p.pos++
			return l, nil
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		if len(l.Items) == 0 || TypeOf(v) == l.Type {
			l.Append(v)
		}
		p.skipWS()
		if p.pos < len(p.s) && p.s[p.pos] == ',' {
			p.pos++
		}
	}
}

func parseScalar(tok string) any {
	switch strings.ToLower(tok) {
	case "true":
		return int8(1)
	case "false":
		return int8(0)
	}
	last := tok[len(tok)-1]
	body := tok[:len(tok)-1]
	switch last {
	case 'b', 'B':
		if n, err := strconv.ParseInt(body, 10, 64); err == nil {
			return int8(n)
		}
	case 's', 'S':
		if n, err := strconv.ParseInt(body, 10, 64); err == nil {
			return int16(n)
		}
	case 'l', 'L':
		if n, err := strconv.ParseInt(body, 10, 64); err == nil {
			return n
		}
	case 'f', 'F':
		if f, err := strconv.ParseFloat(body, 64); err == nil {
			return float32(f)
		}
	case 'd', 'D':
		if f, err := strconv.ParseFloat(body, 64); err == nil {
			return f
		}
	}
	if n, err := strconv.ParseInt(tok, 10, 64); err == nil {
		return int32(n)
	}
	if strings.ContainsAny(tok, ".eE") {
		if f, err := strconv.ParseFloat(tok, 64); err == nil {
			return f
		}
	}
	return tok
}

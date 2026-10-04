// Command benchchart draws the README benchmark chart (docs/bench-light.svg
// and docs/bench-dark.svg) from measured conversion times.
//
//	go run ./tools/benchchart
package main

import (
	"fmt"
	"os"
	"strings"
)

type row struct {
	label        string
	amulet, ours float64 // seconds
}

// Test world (Java 26.2, 734 chunks), same PC (Core Ultra 7 255H).
var rows = []row{
	{"Java版 → 統合版", 17.0, 0.104},
	{"統合版 → Java版", 14.5, 0.111},
}

type theme struct {
	name                         string
	surface, text, text2, muted  string
	grid, baseline, ours, amulet string
}

var themes = []theme{
	{"light", "#fcfcfb", "#0b0b0b", "#52514e", "#898781", "#e1e0d9", "#c3c2b7", "#2a78d6", "#eb6834"},
	{"dark", "#1a1a19", "#ffffff", "#c3c2b7", "#898781", "#2c2c2a", "#383835", "#3987e5", "#d95926"},
}

const (
	width   = 760
	left    = 150.0 // start of the bars
	right   = 600.0 // end of the scale (room for labels after it)
	barH    = 20.0
	gap     = 2.0  // surface gap between the bars of a group
	groupGp = 30.0 // space between groups
	top     = 78.0
	maxSec  = 18.0
)

func x(sec float64) float64 { return left + sec/maxSec*(right-left) }

// bar draws a horizontal bar anchored at the baseline with a 4px rounded end.
func bar(sb *strings.Builder, y, sec float64, fill string) {
	w := max(x(sec)-left, 3) // keep tiny values visible
	r := min(4.0, w/2)
	fmt.Fprintf(sb, `<path d="M%.1f %.1f H%.1f a%.1f %.1f 0 0 1 %.1f %.1f V%.1f a%.1f %.1f 0 0 1 %.1f %.1f H%.1f Z" fill="%s"/>`+"\n",
		left, y, left+w-r, r, r, r, r, y+barH-r, r, r, -r, r, left, fill)
}

func esc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func render(t theme) string {
	var sb strings.Builder
	height := top + float64(len(rows))*(2*barH+gap+groupGp) + 30
	fmt.Fprintf(&sb, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%.0f" viewBox="0 0 %d %.0f" role="img" aria-labelledby="t d">`+"\n", width, height, width, height)
	fmt.Fprintf(&sb, `<title id="t">同じワールドの変換時間: mcconv と Amulet</title>`+"\n")
	var desc []string
	for _, r := range rows {
		desc = append(desc, fmt.Sprintf("%s: Amulet %.1f 秒、mcconv %.2f 秒", r.label, r.amulet, r.ours))
	}
	fmt.Fprintf(&sb, `<desc id="d">%s (テストワールド 734 チャンク)</desc>`+"\n", esc(strings.Join(desc, "。")))
	fmt.Fprintf(&sb, `<style>text{font-family:system-ui,-apple-system,"Segoe UI","Yu Gothic UI","Hiragino Sans",sans-serif}</style>`+"\n")
	fmt.Fprintf(&sb, `<rect width="100%%" height="100%%" rx="12" fill="%s"/>`+"\n", t.surface)
	fmt.Fprintf(&sb, `<text x="24" y="34" font-size="17" font-weight="600" fill="%s">同じワールドの変換にかかった時間</text>`+"\n", t.text)
	fmt.Fprintf(&sb, `<text x="24" y="55" font-size="12.5" fill="%s">テストワールド (Java 26.2, 734 チャンク) · 短いほど速い</text>`+"\n", t.text2)
	// legend (top right)
	lx := 520.0
	for i, l := range []struct{ name, color string }{{"mcconv", t.ours}, {"Amulet", t.amulet}} {
		ox := lx + float64(i)*110
		fmt.Fprintf(&sb, `<rect x="%.0f" y="26" width="12" height="12" rx="3" fill="%s"/><text x="%.0f" y="36.5" font-size="13" fill="%s">%s</text>`+"\n", ox, l.color, ox+18, t.text, l.name)
	}
	// grid
	axisBottom := height - 34
	for s := 0.0; s <= 15; s += 5 {
		gx := x(s)
		stroke := t.grid
		if s == 0 {
			stroke = t.baseline
		}
		fmt.Fprintf(&sb, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1"/>`+"\n", gx, top-10, gx, axisBottom, stroke)
		fmt.Fprintf(&sb, `<text x="%.1f" y="%.1f" font-size="11.5" fill="%s" text-anchor="middle">%.0f 秒</text>`+"\n", gx, axisBottom+16, t.muted, s)
	}
	y := top
	for _, r := range rows {
		fmt.Fprintf(&sb, `<text x="24" y="%.1f" font-size="13.5" font-weight="600" fill="%s">%s</text>`+"\n", y+barH+gap/2+5, t.text, esc(r.label))
		bar(&sb, y, r.ours, t.ours)
		fmt.Fprintf(&sb, `<text x="%.1f" y="%.1f" font-size="13" font-weight="700" fill="%s">%.2f 秒 <tspan font-weight="400" fill="%s">— Amulet の約 %.0f 倍速</tspan></text>`+"\n",
			x(r.ours)+10, y+barH-5.5, t.text, r.ours, t.text2, r.amulet/r.ours)
		y2 := y + barH + gap
		bar(&sb, y2, r.amulet, t.amulet)
		fmt.Fprintf(&sb, `<text x="%.1f" y="%.1f" font-size="13" fill="%s">%.1f 秒</text>`+"\n", x(r.amulet)+8, y2+barH-5.5, t.text2, r.amulet)
		y = y2 + barH + groupGp
	}
	sb.WriteString("</svg>\n")
	return sb.String()
}

func main() {
	for _, t := range themes {
		p := "docs/bench-" + t.name + ".svg"
		if err := os.WriteFile(p, []byte(render(t)), 0o644); err != nil {
			panic(err)
		}
		fmt.Println("wrote", p)
	}
}

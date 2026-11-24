package gui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mcconv/internal/convert"
	"mcconv/internal/translate"
)

// ReplaceRow is one block replacement entered on the page.
type ReplaceRow struct {
	From string `json:"from"`
	To   string `json:"to"`
}

var tablesCache struct {
	sync.Mutex
	dir   string
	stamp time.Time // modification time of tables.json.gz (zero: built-in)
	t     *translate.Tables
}

// tables returns the tables conversions use (updated or built-in), cached
// until tables.json.gz changes.
func (a *App) tables() (*translate.Tables, error) {
	c := &tablesCache
	c.Lock()
	defer c.Unlock()
	p := filepath.Join(a.dataDir, "tables.json.gz")
	var stamp time.Time
	if fi, err := os.Stat(p); err == nil {
		stamp = fi.ModTime()
	}
	if c.t != nil && c.dir == a.dataDir && c.stamp.Equal(stamp) {
		return c.t, nil
	}
	src := translate.BuiltinTables()
	if !stamp.IsZero() {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		src = b
	}
	t, err := translate.Parse(src)
	if err != nil {
		return nil, err
	}
	c.dir, c.stamp, c.t = a.dataDir, stamp, t
	return t, nil
}

// BlockIDs lists the blocks of both editions (ids without "minecraft:")
// with their Japanese names, for the suggestions of the replacement rows.
func (a *App) BlockIDs() (map[string][]BlockInfo, error) {
	t, err := a.tables()
	if err != nil {
		return nil, err
	}
	lang := javaLangJA()
	var java, bedrock []BlockInfo
	for _, n := range t.JavaBlockNames() {
		java = append(java, BlockInfo{ID: short(n), Name: javaName(lang, n)})
	}
	for _, n := range t.BedrockBlockNames() {
		bi := BlockInfo{ID: short(n)}
		if j, ok := t.JavaOfBedrock(n); ok {
			bi.Name = javaName(lang, j)
		} else if t.HasJavaBlock(n) {
			bi.Name = javaName(lang, n)
		} else {
			bi.Name = bedrockOnlyJA[short(n)]
		}
		bedrock = append(bedrock, bi)
	}
	return map[string][]BlockInfo{"java": java, "bedrock": bedrock}, nil
}

// ReplaceCheck has, for each row, the reason it cannot be used ("" if it
// can) and a notice that does not stop the conversion (add-on / mod blocks).
type ReplaceCheck struct {
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
}

// CheckReplace checks the rows. target is the edition written.
func (a *App) CheckReplace(target string, rows []ReplaceRow) (ReplaceCheck, error) {
	t, err := a.tables()
	if err != nil {
		return ReplaceCheck{}, err
	}
	toBedrock := target == "bedrock"
	srcEd, dstEd := "Java版", "統合版"
	srcKind, dstKind := "MOD", "アドオン"
	if !toBedrock {
		srcEd, dstEd = dstEd, srcEd
		srcKind, dstKind = dstKind, srcKind
	}
	out := ReplaceCheck{Errors: make([]string, len(rows)), Warnings: make([]string, len(rows))}
	seen := map[string]bool{}
	for i, r := range rows {
		from, to := normID(r.From), normID(r.To)
		switch {
		case from == "" && to == "":
		case from == "" || to == "":
			out.Errors[i] = "元のブロックと置き換え先の両方を入力してください"
		case seen[from]:
			out.Errors[i] = fmt.Sprintf("「%s」は上の行でも指定しています", short(from))
		case strings.Contains(from, "["):
			out.Errors[i] = "状態 ([…]) は置き換え先にだけ書けます"
		default:
			if err := convert.CheckTarget(to); err != nil {
				if convert.IsCustom(to) {
					out.Errors[i] = "状態は [名前=値,名前=値] の形で書いてください (例: [stage=1])"
				} else {
					out.Errors[i] = fmt.Sprintf("状態 ([…]) は %s のブロックにだけ書けます。標準のブロックは元のブロックから引き継ぎます", dstKind)
				}
				break
			}
			badFrom, badTo := convert.ReplaceProblem(t, toBedrock, from, to)
			if badFrom {
				out.Errors[i] = fmt.Sprintf("「%s」は %s のブロックIDではありません", short(from), srcEd)
			} else if badTo {
				out.Errors[i] = fmt.Sprintf("「%s」は %s のブロックIDではありません", short(to), dstEd)
			}
		}
		if out.Errors[i] == "" && from != "" && to != "" {
			var w []string
			if convert.IsCustom(from) {
				w = append(w, fmt.Sprintf("「%s」は標準のブロックではないので、%s のブロックとして扱います。", from, srcKind))
			}
			if convert.IsCustom(to) {
				id, _, _ := strings.Cut(to, "[")
				w = append(w, fmt.Sprintf("「%s」は標準のブロックではないので、%s のブロックとしてそのまま書き込みます。その %s が入っていないと正しく表示されません。", id, dstKind, dstKind))
			}
			out.Warnings[i] = strings.Join(w, " ")
		}
		if from != "" {
			seen[from] = true
		}
	}
	return out, nil
}

// normID lower-cases the id (not a state list after it) and adds
// "minecraft:".
func normID(s string) string {
	s = strings.TrimSpace(s)
	id, state, ok := strings.Cut(s, "[")
	id = strings.ToLower(strings.TrimSpace(id))
	if id != "" && !strings.Contains(id, ":") {
		id = "minecraft:" + id
	}
	if ok {
		return id + "[" + state
	}
	return id
}

func short(id string) string { return strings.TrimPrefix(id, "minecraft:") }

// replaceArg checks the rows and returns the -replace value ("" if none).
func (a *App) replaceArg(target string, rows []ReplaceRow) (string, error) {
	chk, err := a.CheckReplace(target, rows)
	if err != nil {
		return "", err
	}
	var parts []string
	for i, r := range rows {
		if m := chk.Errors[i]; m != "" {
			return "", fmt.Errorf("ブロックの置き換え %d 行目: %s", i+1, m)
		}
		if from, to := normID(r.From), normID(r.To); from != "" {
			parts = append(parts, from+"="+to)
		}
	}
	return strings.Join(parts, ","), nil
}

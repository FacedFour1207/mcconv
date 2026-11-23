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

// CheckReplace returns a message for each row that cannot be used ("" if
// the row is fine or empty). target is the edition written.
func (a *App) CheckReplace(target string, rows []ReplaceRow) ([]string, error) {
	t, err := a.tables()
	if err != nil {
		return nil, err
	}
	toBedrock := target == "bedrock"
	srcEd, dstEd := "Java版", "統合版"
	if !toBedrock {
		srcEd, dstEd = dstEd, srcEd
	}
	out := make([]string, len(rows))
	seen := map[string]bool{}
	for i, r := range rows {
		from, to := normID(r.From), normID(r.To)
		switch {
		case from == "" && to == "":
		case from == "" || to == "":
			out[i] = "元のブロックと置き換え先の両方を入力してください"
		case seen[from]:
			out[i] = fmt.Sprintf("「%s」は上の行でも指定しています", short(from))
		default:
			badFrom, badTo := convert.ReplaceProblem(t, toBedrock, from, to)
			if badFrom {
				out[i] = fmt.Sprintf("「%s」は %s のブロックIDではありません", short(from), srcEd)
			} else if badTo {
				out[i] = fmt.Sprintf("「%s」は %s のブロックIDではありません", short(to), dstEd)
			}
		}
		if from != "" {
			seen[from] = true
		}
	}
	return out, nil
}

func normID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || strings.Contains(s, ":") {
		return s
	}
	return "minecraft:" + s
}

func short(id string) string { return strings.TrimPrefix(id, "minecraft:") }

// replaceArg checks the rows and returns the -replace value ("" if none).
func (a *App) replaceArg(target string, rows []ReplaceRow) (string, error) {
	msgs, err := a.CheckReplace(target, rows)
	if err != nil {
		return "", err
	}
	var parts []string
	for i, r := range rows {
		if msgs[i] != "" {
			return "", fmt.Errorf("ブロックの置き換え %d 行目: %s", i+1, msgs[i])
		}
		if from, to := normID(r.From), normID(r.To); from != "" {
			parts = append(parts, from+"="+to)
		}
	}
	return strings.Join(parts, ","), nil
}

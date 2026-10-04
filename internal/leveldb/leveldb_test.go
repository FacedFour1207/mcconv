package leveldb

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

const testWorld = "../../test world(BE)/db"

func TestRoundTrip(t *testing.T) {
	if _, err := os.Stat(testWorld); err != nil {
		t.Skip("test world not present")
	}
	db, err := Open(testWorld)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	it := db.NewIterator()
	src := map[string][]byte{}
	var keys [][]byte
	for it.Next() {
		k := append([]byte(nil), it.Key()...)
		src[string(k)] = append([]byte(nil), it.Value()...)
		keys = append(keys, k)
	}
	if it.Err() != nil {
		t.Fatal(it.Err())
	}
	t.Logf("read %d keys", len(keys))
	for i := 1; i < len(keys); i++ {
		if bytes.Compare(keys[i-1], keys[i]) >= 0 {
			t.Fatalf("keys out of order at %d", i)
		}
	}
	out := filepath.Join(t.TempDir(), "db")
	if d := os.Getenv("LDB_OUT"); d != "" {
		out = d
		os.RemoveAll(out)
	}
	bl := NewBulkLoader(filepath.Join(t.TempDir(), "tmp"), 1000) // force spills
	for k, v := range src {
		bl.Put([]byte(k), v)
	}
	if err := bl.Finish(out, Options{CompressionLevel: 1}); err != nil {
		t.Fatal(err)
	}
	db2, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	n := 0
	it2 := db2.NewIterator()
	for it2.Next() {
		n++
		if !bytes.Equal(src[string(it2.Key())], it2.Value()) {
			t.Fatalf("value mismatch for %x", it2.Key())
		}
	}
	if n != len(src) {
		t.Fatalf("got %d keys want %d", n, len(src))
	}
	for k, v := range src {
		got, ok, err := db2.Get([]byte(k))
		if err != nil || !ok || !bytes.Equal(got, v) {
			t.Fatalf("Get %x failed: ok=%v err=%v", k, ok, err)
		}
	}
}

func TestStreams(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")
	b, err := NewDBBuilder(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	// two disjoint ranges written by separate streams at level 6, plus extras at level 5
	for s := 0; s < 2; s++ {
		st := b.NewStream(6)
		for i := 0; i < 50000; i++ {
			k := []byte{byte('A' + s), byte(i >> 16), byte(i >> 8), byte(i)}
			v := bytes.Repeat([]byte{byte(i)}, 100+i%300)
			if err := st.Add(k, v); err != nil {
				t.Fatal(err)
			}
			want[string(k)] = string(v)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}
	ex := b.NewStream(5)
	for _, k := range []string{"0extra", "Bzzz", "zz"} {
		ex.Add([]byte(k), []byte("x"+k))
		want[k] = "x" + k
	}
	ex.Close()
	if err := b.Finish(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	n := 0
	it := db.NewIterator()
	var prev []byte
	for it.Next() {
		if prev != nil && bytes.Compare(prev, it.Key()) >= 0 {
			t.Fatal("order")
		}
		prev = append(prev[:0], it.Key()...)
		if want[string(it.Key())] != string(it.Value()) {
			t.Fatalf("value mismatch %x", it.Key())
		}
		n++
	}
	if n != len(want) || len(db.tables) < 3 {
		t.Fatalf("got %d keys (want %d) in %d tables", n, len(want), len(db.tables))
	}
	for k, v := range want {
		got, ok, _ := db.Get([]byte(k))
		if !ok || string(got) != v {
			t.Fatalf("get %x", k)
		}
	}
}

func TestRanges(t *testing.T) {
	path := testWorld
	if p := os.Getenv("LDB_RANGE_DB"); p != "" {
		path = p
	}
	if _, err := os.Stat(path); err != nil {
		t.Skip()
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var all []string
	it := db.NewIterator()
	for it.Next() {
		all = append(all, string(it.Key()))
	}
	splits := db.SplitKeys(8, 8)
	bounds := append([][]byte{nil}, splits...)
	bounds = append(bounds, nil)
	var got []string
	for i := 0; i+1 < len(bounds); i++ {
		r := db.NewRangeIterator(bounds[i], bounds[i+1])
		for r.Next() {
			got = append(got, string(r.Key()))
		}
	}
	t.Logf("%d keys, %d splits", len(all), len(splits))
	if len(got) != len(all) {
		t.Fatalf("ranges returned %d keys, want %d (splits %d)", len(got), len(all), len(splits))
	}
	for i := range got {
		if got[i] != all[i] {
			t.Fatalf("key %d differs", i)
		}
	}
}

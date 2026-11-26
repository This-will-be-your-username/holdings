package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func seedMigrateStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SeedTestData(s, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	return s
}

// What gets saved must survive export → import unchanged.
func TestBundleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := seedMigrateStore(t, filepath.Join(dir, "a.json"))
	src.Update(func(db *DB) error {
		db.Settings.OtherIncome = 91234
		db.Owners = append(db.Owners, Owner{Name: "Sam"})
		return nil
	})
	var buf bytes.Buffer
	tg := &TelegramConfig{Enabled: true, Token: "123:abc", ChatID: 42, PendingChat: 7}
	src.View(func(db *DB) {
		if err := WriteBundle(&buf, db, "real", tg, time.Now()); err != nil {
			t.Fatal(err)
		}
	})

	db, b, err := ReadBundle(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if b.Telegram == nil || b.Telegram.Token != "123:abc" || b.Telegram.PendingChat != 0 {
		t.Fatalf("telegram not carried as expected: %+v", b.Telegram)
	}
	dst, _ := OpenStore(filepath.Join(dir, "b.json"))
	if _, err := dst.Replace(db, time.Now()); err != nil {
		t.Fatal(err)
	}
	// reopen from disk, as a fresh installation would
	dst, err = OpenStore(filepath.Join(dir, "b.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want, got []byte
	src.View(func(db *DB) { want, _ = json.Marshal(db) })
	dst.View(func(db *DB) { got, _ = json.Marshal(db) })
	if !bytes.Equal(want, got) {
		t.Fatal("imported data differs from the exported data")
	}
}

func TestReadBundleAcceptsPlainAndGzippedDataFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fundtrack.json")
	s := seedMigrateStore(t, path)
	var n int
	s.View(func(db *DB) { n = len(db.Txns) })
	raw, _ := os.ReadFile(path)

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(raw)
	zw.Close()

	for name, in := range map[string][]byte{"plain": raw, "gzip": gz.Bytes(), "bom": append([]byte("\xef\xbb\xbf"), raw...)} {
		db, _, err := ReadBundle(bytes.NewReader(in))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(db.Txns) != n {
			t.Fatalf("%s: %d transactions, want %d", name, len(db.Txns), n)
		}
	}
}

func TestReadBundleRejectsBadFiles(t *testing.T) {
	cases := map[string]string{
		"empty":   "",
		"notjson": "date,symbol\n2024-01-01,VGS",
		"other":   `{"hello":"world"}`,
		"format":  `{"format":"something-else","version":1,"data":{}}`,
		"newer":   `{"format":"holdings-backup","version":99,"data":{}}`,
		"nodata":  `{"format":"holdings-backup","version":1}`,
		"baddate": `{"txns":[{"id":"a","date":"yesterday","symbol":"VGS.AX","type":"buy","units":1,"price":1}]}`,
		"dupid":   `{"txns":[{"id":"a","date":"2024-01-01","symbol":"X","type":"buy","units":1,"price":1},{"id":"a","date":"2024-01-02","symbol":"X","type":"buy","units":1,"price":1}]}`,
	}
	for name, in := range cases {
		if _, _, err := ReadBundle(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestReplaceKeepsBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fundtrack.json")
	s := seedMigrateStore(t, path)
	before, _ := os.ReadFile(path)
	backup, err := s.Replace(&DB{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	kept, err := os.ReadFile(backup)
	if err != nil || !bytes.Equal(kept, before) {
		t.Fatalf("backup %q doesn't hold the previous data (%v)", backup, err)
	}
	s.View(func(db *DB) {
		if len(db.Txns) != 0 || db.Instruments == nil {
			t.Fatal("data not replaced, or not initialised")
		}
	})
}

// Through the web: export from one app, import into another.
func TestExportImportOverHTTP(t *testing.T) {
	t.Setenv("TEST_MODE", "1")
	from, err := newApp(filepath.Join(t.TempDir(), "fundtrack.json"))
	if err != nil {
		t.Fatal(err)
	}
	srvA := httptest.NewServer(from.routes())
	defer srvA.Close()
	resp, err := http.Get(srvA.URL + "/export/backup.json")
	if err != nil {
		t.Fatal(err)
	}
	export, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Content-Disposition"), "holdings-test-backup") {
		t.Fatalf("unexpected filename: %s", resp.Header.Get("Content-Disposition"))
	}

	t.Setenv("TEST_MODE", "0")
	to, err := newApp(filepath.Join(t.TempDir(), "fundtrack.json"))
	if err != nil {
		t.Fatal(err)
	}
	srvB := httptest.NewServer(to.routes())
	defer srvB.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	post := func(confirm bool) string {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("file", "backup.json")
		fw.Write(export)
		if confirm {
			mw.WriteField("confirm", "1")
		}
		mw.Close()
		resp, err := client.Post(srvB.URL+"/settings/import", mw.FormDataContentType(), &body)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get("Location")
	}
	if loc := post(false); !strings.Contains(loc, "err=") {
		t.Fatalf("import without confirmation should be refused, got %s", loc)
	}
	if loc := post(true); !strings.Contains(loc, "msg=Imported") {
		t.Fatalf("import failed: %s", loc)
	}
	var want, got []Txn
	from.st().View(func(db *DB) { want = db.Txns })
	to.st().View(func(db *DB) { got = db.Txns })
	if len(got) == 0 || !reflect.DeepEqual(want, got) {
		t.Fatalf("imported %d transactions, want %d identical", len(got), len(want))
	}
}

func TestCLIExportImport(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src", "fundtrack.json")
	seedMigrateStore(t, src)
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.WriteFile(filepath.Join(dir, "src", "telegram.json"), []byte(`{"enabled":true,"token":"1:x","chat_id":5}`), 0o600)
	out := filepath.Join(dir, "export.json")
	var stderr bytes.Buffer
	if ok, code := runCLI([]string{"export", "-telegram", out}, src, io.Discard, &stderr); !ok || code != 0 {
		t.Fatalf("export: %d %s", code, stderr.String())
	}
	dst := filepath.Join(dir, "dst", "fundtrack.json")
	if ok, code := runCLI([]string{"import", "--telegram", out}, dst, io.Discard, &stderr); !ok || code != 0 {
		t.Fatalf("import: %d %s", code, stderr.String())
	}
	a, _ := os.ReadFile(src)
	b, _ := os.ReadFile(dst)
	var da, db DB
	json.Unmarshal(a, &da)
	json.Unmarshal(b, &db)
	if len(db.Txns) == 0 || !reflect.DeepEqual(da.Txns, db.Txns) {
		t.Fatal("CLI import didn't reproduce the transactions")
	}
	var tg tgState
	tg.load(filepath.Join(dir, "dst"))
	if tg.cfg.Token != "1:x" || tg.cfg.ChatID != 5 {
		t.Fatalf("telegram not restored: %+v", tg.cfg)
	}
	if ok, _ := runCLI(nil, dst, io.Discard, io.Discard); ok {
		t.Fatal("no arguments should run the web app")
	}
	if ok, code := runCLI([]string{"bogus"}, dst, io.Discard, io.Discard); !ok || code != 2 {
		t.Fatal("unknown command should fail with usage")
	}
}

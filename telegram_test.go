package main

import (
	"bytes"
	"encoding/json"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeTelegram records what the bot sends and answers like the Bot API.
type fakeTelegram struct {
	mu     sync.Mutex
	texts  []string
	photos []string // captions
	pngs   [][]byte
	calls  []string
	kb     []string // callback data of the last keyboard
	srv    *httptest.Server
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	f := &fakeTelegram{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, method)
		switch method {
		case "getMe":
			if !strings.Contains(r.URL.Path, "/botGOOD") {
				w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
				return
			}
			w.Write([]byte(`{"ok":true,"result":{"username":"holdings_test_bot"}}`))
		case "sendMessage":
			var p struct {
				Text   string `json:"text"`
				Markup struct {
					Kb [][]tgButton `json:"inline_keyboard"`
				} `json:"reply_markup"`
			}
			json.NewDecoder(r.Body).Decode(&p)
			f.texts = append(f.texts, p.Text)
			f.kb = nil
			for _, row := range p.Markup.Kb {
				for _, b := range row {
					f.kb = append(f.kb, b.Data)
				}
			}
			w.Write([]byte(`{"ok":true,"result":{}}`))
		case "getUpdates":
			f.mu.Unlock()
			time.Sleep(150 * time.Millisecond)
			f.mu.Lock()
			w.Write([]byte(`{"ok":true,"result":[]}`))
		case "sendPhoto":
			r.ParseMultipartForm(8 << 20)
			f.photos = append(f.photos, r.FormValue("caption"))
			file, _, err := r.FormFile("photo")
			if err == nil {
				b, _ := io.ReadAll(file)
				f.pngs = append(f.pngs, b)
			}
			w.Write([]byte(`{"ok":true,"result":{}}`))
		default:
			w.Write([]byte(`{"ok":true,"result":true}`))
		}
	}))
	old := telegramBase
	telegramBase = f.srv.URL
	t.Cleanup(func() { telegramBase = old; f.srv.Close() })
	return f
}

func testApp(t *testing.T) *App {
	t.Helper()
	t.Setenv("TEST_MODE", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "fundtrack.json"))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (a *App) tgAuthorise(chat int64) {
	a.tg.mu.Lock()
	a.tg.cfg.ChatID = chat
	a.tg.mu.Unlock()
}

func TestSparklinePNG(t *testing.T) {
	for _, up := range []bool{true, false} {
		b, err := sparklinePNG([]float64{10, 10.5, 10.2, 11, 11.4}, up)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		if img.Bounds().Dx() != 480 || img.Bounds().Dy() != 160 {
			t.Fatalf("unexpected size %v", img.Bounds())
		}
	}
	if _, err := sparklinePNG([]float64{1}, true); err == nil {
		t.Fatal("one point should be refused")
	}
	if _, err := sparklinePNG([]float64{5, 5, 5}, true); err != nil { // a flat week must not divide by zero
		t.Fatal(err)
	}
}

func TestWeekSeries(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	db := &DB{Prices: map[string]map[string]float64{"X": {"2026-11-01": 1, "2026-11-19": 2, "2026-11-20": 3, "2026-11-24": 4}},
		Manual: map[string]map[string]float64{"X": {"2026-11-24": 5}}, Quotes: map[string]Quote{"X": {Price: 6, Date: "2026-11-25"}}}
	got := weekSeries(db, "X", now)
	if len(got) != 4 || got[0].Date != "2026-11-19" || got[2].Value != 5 || got[3].Value != 6 {
		t.Fatalf("unexpected week %+v", got)
	}
	thin := &DB{Prices: map[string]map[string]float64{"Y": {"2026-10-01": 1, "2026-10-02": 2, "2026-10-03": 3}}}
	if got := weekSeries(thin, "Y", now); len(got) != 3 {
		t.Fatalf("a thin window should fall back to the last saved prices, got %+v", got)
	}
}

func TestTelegramCommands(t *testing.T) {
	f := newFakeTelegram(t)
	a := testApp(t)
	a.tgAuthorise(42)
	c := newTG("GOOD", 5*time.Second)

	a.tgCommand(c, 42, "/prices")
	f.mu.Lock()
	if len(f.texts) != 1 || !strings.Contains(f.texts[0], "VGS") || !strings.Contains(f.texts[0], "graphs of the last week") {
		t.Fatalf("snapshot should list holdings and offer graphs: %q", f.texts)
	}
	hasAll := false
	for _, d := range f.kb {
		hasAll = hasAll || d == "g:ALL"
	}
	if !hasAll || len(f.kb) < 2 {
		t.Fatalf("expected an all-holdings button and one per holding: %v", f.kb)
	}
	f.mu.Unlock()

	a.tgCommand(c, 42, "/graph VGS")
	f.mu.Lock()
	if len(f.photos) != 1 || !strings.Contains(f.photos[0], "VGS") {
		t.Fatalf("expected one VGS graph: %v", f.photos)
	}
	if _, err := png.Decode(bytes.NewReader(f.pngs[0])); err != nil {
		t.Fatalf("graph is not a PNG: %v", err)
	}
	f.photos, f.pngs = nil, nil
	f.mu.Unlock()

	a.tgCommand(c, 42, "/graph")
	f.mu.Lock()
	if len(f.photos) < 2 {
		t.Fatalf("/graph should send one per holding, got %d", len(f.photos))
	}
	f.mu.Unlock()

	a.tgCommand(c, 42, "/graph NOPE")
	f.mu.Lock()
	if !strings.Contains(f.texts[len(f.texts)-1], "couldn't find") {
		t.Fatalf("unknown holding should be explained: %q", f.texts[len(f.texts)-1])
	}
	f.mu.Unlock()

	a.tgCommand(c, 42, "/refresh") // test mode: nothing is fetched, but it must still answer with a snapshot
	f.mu.Lock()
	n := len(f.texts)
	if n < 3 || !strings.Contains(f.texts[n-1], "Test mode") || !strings.Contains(f.texts[n-1], "Prices") {
		t.Fatalf("refresh should report and then snapshot: %q", f.texts)
	}
	f.mu.Unlock()
}

func TestTelegramCallbackAndAuthorisation(t *testing.T) {
	f := newFakeTelegram(t)
	a := testApp(t)
	c := newTG("GOOD", 5*time.Second)

	// a stranger is refused, and recorded as pending so the owner can approve from Settings
	var u tgUpdate
	json.Unmarshal([]byte(`{"update_id":1,"message":{"text":"/prices","chat":{"id":99,"type":"private","first_name":"Eve","username":"eve"}}}`), &u)
	a.telegramHandle(c, u)
	f.mu.Lock()
	if len(f.texts) != 1 || !strings.Contains(f.texts[0], "private") || strings.Contains(f.texts[0], "VGS") {
		t.Fatalf("stranger should be refused without data: %q", f.texts)
	}
	f.mu.Unlock()
	if v := a.telegramView(); v.PendingChat != 99 || !strings.Contains(v.PendingName, "Eve") {
		t.Fatalf("pending chat not recorded: %+v", v)
	}

	// a stranger's button press does nothing
	u = tgUpdate{}
	json.Unmarshal([]byte(`{"update_id":2,"callback_query":{"id":"1","data":"g:ALL","message":{"chat":{"id":99,"type":"private"}}}}`), &u)
	a.telegramHandle(c, u)
	f.mu.Lock()
	if len(f.photos) != 0 {
		t.Fatal("unapproved chat received graphs")
	}
	f.mu.Unlock()

	// approved via the settings page
	srv := httptest.NewServer(a.routes())
	defer srv.Close()
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(path string, v url.Values) string {
		resp, err := hc.PostForm(srv.URL+path, v)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get("Location")
	}
	if loc := post("/settings/telegram/approve", url.Values{"chat": {"123"}}); !strings.Contains(loc, "err=") {
		t.Fatalf("approving a chat that isn't pending must fail: %s", loc)
	}
	if loc := post("/settings/telegram/approve", url.Values{"chat": {"99"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("approve failed: %s", loc)
	}
	u = tgUpdate{}
	json.Unmarshal([]byte(`{"update_id":3,"callback_query":{"id":"2","data":"g:ALL","message":{"chat":{"id":99,"type":"private"}}}}`), &u)
	a.telegramHandle(c, u)
	f.mu.Lock()
	if len(f.photos) < 2 {
		t.Fatalf("approved chat's button press should send graphs, got %d", len(f.photos))
	}
	f.mu.Unlock()
}

func TestTelegramSettingsFlow(t *testing.T) {
	f := newFakeTelegram(t)
	a := testApp(t)
	defer func() { a.tg.mu.Lock(); a.tg.running = false; a.tg.mu.Unlock() }()
	srv := httptest.NewServer(a.routes())
	defer srv.Close()
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(path string, v url.Values) string {
		resp, err := hc.PostForm(srv.URL+path, v)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		l, _ := url.QueryUnescape(resp.Header.Get("Location"))
		return l
	}

	if loc := post("/settings/telegram", url.Values{"token": {"BAD"}, "enabled": {"1"}}); !strings.Contains(loc, "err=") {
		t.Fatalf("a rejected token must not be saved: %s", loc)
	}
	if a.telegramView().HasToken {
		t.Fatal("bad token was saved")
	}
	if loc := post("/settings/telegram", url.Values{"enabled": {"1"}}); !strings.Contains(loc, "err=") {
		t.Fatalf("enabling without a token must fail: %s", loc)
	}
	if loc := post("/settings/telegram", url.Values{"token": {"GOOD"}, "enabled": {"1"}, "chat_id": {"abc"}}); !strings.Contains(loc, "err=") {
		t.Fatalf("a non-numeric chat id must fail: %s", loc)
	}
	if loc := post("/settings/telegram", url.Values{"token": {"GOOD"}, "enabled": {"1"}, "chat_id": {"42"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("save failed: %s", loc)
	}
	a.tg.mu.Lock()
	if a.tg.cancel != nil {
		defer a.tg.cancel()
	}
	a.tg.mu.Unlock()
	if v := a.telegramView(); !v.HasToken || v.ChatID != 42 || !v.Running || v.BotName != "holdings_test_bot" {
		t.Fatalf("unexpected state: %+v", v)
	}

	// the saved token is in its own owner-only file, never in the main data file, and never in the page
	b, err := readFile(filepath.Join(a.dir, "telegram.json"))
	if err != nil || !strings.Contains(string(b), "GOOD") {
		t.Fatalf("telegram.json: %v %s", err, b)
	}
	resp, _ := http.Get(srv.URL + "/settings")
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(page), "GOOD") && !strings.Contains(string(page), "…GOOD") {
		t.Fatal("the page leaks the whole token")
	}
	if !strings.Contains(string(page), "Telegram bot") || !strings.Contains(string(page), "@holdings_test_bot") {
		t.Fatalf("settings page missing status: %s", page)
	}

	if loc := post("/settings/telegram/test", nil); !strings.Contains(loc, "msg=") {
		t.Fatalf("test message failed: %s", loc)
	}
	f.mu.Lock()
	if len(f.texts) == 0 || !strings.Contains(f.texts[len(f.texts)-1], "connected") {
		t.Fatalf("test message not delivered: %q", f.texts)
	}
	f.mu.Unlock()

	// a restart reads the saved configuration back
	a2, err := newApp(filepath.Join(a.dir, "fundtrack.json"))
	if err != nil {
		t.Fatal(err)
	}
	if v := a2.telegramView(); !v.HasToken || v.ChatID != 42 || !v.Enabled {
		t.Fatalf("config not reloaded: %+v", v)
	}

	post("/settings/telegram/forget", nil)
	if v := a.telegramView(); v.HasToken || v.ChatID != 0 || v.Running {
		t.Fatalf("settings not cleared: %+v", v)
	}
}

func TestTelegramErrorsHideToken(t *testing.T) {
	c := newTG("SECRET123", time.Second)
	err := c.cleanErr(io.ErrUnexpectedEOF)
	if err == nil {
		t.Fatal("nil error")
	}
	e := c.cleanErr(&url.Error{Op: "Post", URL: "https://api.telegram.org/botSECRET123/getMe", Err: io.EOF})
	if strings.Contains(e.Error(), "SECRET123") {
		t.Fatalf("token leaked: %v", e)
	}
}

func readFile(p string) ([]byte, error) { return os.ReadFile(p) }

func TestTelegramGroups(t *testing.T) {
	f := newFakeTelegram(t)
	a := testApp(t)
	a.tgAuthorise(42)
	c := newTG("GOOD", 5*time.Second)

	// pick two listed holdings for the group; the rest stay ungrouped
	var listed []string
	a.st().View(func(db *DB) {
		for _, h := range Compute(db, liveNow()).Holdings {
			if !h.Private {
				listed = append(listed, h.Symbol)
			}
		}
	})
	if len(listed) < 3 {
		t.Fatalf("test data needs at least three listed holdings, got %v", listed)
	}

	srv := httptest.NewServer(a.routes())
	defer srv.Close()
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.PostForm(srv.URL+"/settings/telegram/groups", url.Values{
		"group:" + listed[0]: {"  Super   fund "},
		"group:" + listed[1]: {"super FUND"}, // a different case joins the same group
		"group:NOT-A-SYMBOL": {"Ignored"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc, _ := url.QueryUnescape(resp.Header.Get("Location")); !strings.Contains(loc, "2 changed") {
		t.Fatalf("expected two holdings grouped: %s", loc)
	}
	var group string
	a.st().View(func(db *DB) {
		g := db.groupNames()
		if len(g) != 1 || !strings.EqualFold(g[0], "Super fund") {
			t.Fatalf("expected one tidied group, got %q", g)
		}
		group = g[0]
	})

	// the settings page shows the editor
	page, err := hc.Get(srv.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if !strings.Contains(string(body), `name="group:`+listed[0]+`" value="`+group+`"`) {
		t.Fatal("settings page should show each holding's group")
	}

	a.tgCommand(c, 42, "/prices")
	f.mu.Lock()
	text := f.texts[len(f.texts)-1]
	iSuper, iOther := strings.Index(text, "📁 <b>"+group+"</b>"), strings.Index(text, "📁 <b>Other</b>")
	if iSuper < 0 || iOther < iSuper {
		t.Fatalf("snapshot should list the group, then Other: %q", text)
	}
	if i := strings.Index(text, "<b>"+short(listed[0])+"</b>"); i < iSuper || i > iOther {
		t.Fatalf("%s should be listed under its group: %q", listed[0], text)
	}
	hasGroupBtn := false
	for _, d := range f.kb {
		hasGroupBtn = hasGroupBtn || d == "g:"+group
	}
	if !hasGroupBtn {
		t.Fatalf("expected a group graph button: %v", f.kb)
	}
	f.photos = nil
	f.mu.Unlock()

	a.tgCommand(c, 42, "/graph super fund")
	f.mu.Lock()
	if len(f.photos) != 2 {
		t.Fatalf("/graph <group> should send the group's two graphs, got %d", len(f.photos))
	}
	f.mu.Unlock()

	// clearing every group brings back the plain list
	resp, _ = hc.PostForm(srv.URL+"/settings/telegram/groups", url.Values{"group:" + listed[0]: {""}, "group:" + listed[1]: {""}})
	resp.Body.Close()
	a.tgCommand(c, 42, "/prices")
	f.mu.Lock()
	if text := f.texts[len(f.texts)-1]; strings.Contains(text, "📁") {
		t.Fatalf("no groups should mean no headings: %q", text)
	}
	f.mu.Unlock()
}

func TestCleanGroup(t *testing.T) {
	if g := cleanGroup("  a   b  "); g != "a b" {
		t.Fatalf("got %q", g)
	}
	long := strings.Repeat("é", 30) // 60 bytes
	if g := cleanGroup(long); len(g) > maxGroupLen || !utf8.ValidString(g) {
		t.Fatalf("long name not trimmed on a rune boundary: %q (%d bytes)", g, len(g))
	}
}

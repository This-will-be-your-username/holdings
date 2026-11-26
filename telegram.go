package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Telegram bot.
//
// The bot lets you check your holdings from your phone: ask it to pull fresh prices, send a snapshot of the latest
// prices, and offer small graphs of the last week. It uses long polling (the app calls out to Telegram), so nothing
// has to be exposed to the internet and the app can stay bound to the loopback address.
//
// Security model: the bot only ever answers ONE chat, the one you approved on the Settings page. Anyone else who
// messages the bot gets a refusal and shows up on the Settings page as a pending request that you may approve or
// ignore. The bot token is kept in its own file (telegram.json beside the data file, owner-only permissions), never
// in the main data file you back up or share.

var telegramBase = "https://api.telegram.org" // tests replace it

// TelegramConfig is what the Settings page edits.
type TelegramConfig struct {
	Enabled bool   `json:"enabled"`
	Token   string `json:"token,omitempty"`
	ChatID  int64  `json:"chat_id,omitempty"` // the one approved chat (0 = nobody approved yet)

	// The most recent chat that wrote to the bot without being approved, so the Settings page can offer to approve it.
	PendingChat int64  `json:"pending_chat,omitempty"`
	PendingName string `json:"pending_name,omitempty"`
}

// tgState is the live side of the bot: configuration plus what the poller is doing.
type tgState struct {
	mu      sync.Mutex
	path    string
	cfg     TelegramConfig
	cancel  context.CancelFunc
	running bool
	botName string
	lastErr string
	lastOK  time.Time
}

func (s *tgState) load(dir string) {
	s.path = filepath.Join(dir, "telegram.json")
	if b, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(b, &s.cfg)
	}
}

func (s *tgState) saveLocked() error {
	b, err := json.MarshalIndent(&s.cfg, "", " ")
	if err != nil {
		return err
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// TelegramView is the status shown on the Settings page. It never includes the token.
type TelegramView struct {
	Enabled, HasToken, Running bool
	TokenHint                  string
	ChatID                     int64
	PendingChat                int64
	PendingName                string
	BotName, LastErr, LastOK   string
}

func (a *App) telegramView() TelegramView {
	s := &a.tg
	s.mu.Lock()
	defer s.mu.Unlock()
	v := TelegramView{Enabled: s.cfg.Enabled, HasToken: s.cfg.Token != "", Running: s.running, ChatID: s.cfg.ChatID,
		PendingChat: s.cfg.PendingChat, PendingName: s.cfg.PendingName, BotName: s.botName, LastErr: s.lastErr}
	if t := s.cfg.Token; len(t) > 4 {
		v.TokenHint = "…" + t[len(t)-4:]
	}
	if !s.lastOK.IsZero() {
		v.LastOK = s.lastOK.Format("2 Jan 15:04:05")
	}
	return v
}

// startTelegram (re)starts the poller to match the saved configuration.
func (a *App) startTelegram() {
	s := &a.tg
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.running = false
	if !s.cfg.Enabled || s.cfg.Token == "" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.running = cancel, true
	token := s.cfg.Token
	go a.telegramLoop(ctx, token)
}

// ---------- Telegram HTTP API ----------

type tgClient struct {
	token string
	http  *http.Client
}

func newTG(token string, timeout time.Duration) *tgClient {
	return &tgClient{token: token, http: &http.Client{Timeout: timeout}}
}

type tgResp struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
}

func (c *tgClient) endpoint(method string) string {
	return telegramBase + "/bot" + c.token + "/" + method
}

// cleanErr stops the token leaking into messages (Go's HTTP errors quote the URL).
func (c *tgClient) cleanErr(err error) error {
	if err == nil || c.token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), c.token, "<token>"))
}

func (c *tgClient) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return c.cleanErr(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return c.cleanErr(err)
	}
	var r tgResp
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("Telegram answered HTTP %d", resp.StatusCode)
	}
	if !r.OK {
		return fmt.Errorf("Telegram: %s (%d)", r.Description, r.ErrorCode)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func (c *tgClient) call(method string, params map[string]any, out any) error {
	return c.callCtx(context.Background(), method, params, out)
}

func (c *tgClient) callCtx(ctx context.Context, method string, params map[string]any, out any) error {
	b, _ := json.Marshal(params)
	req, _ := http.NewRequestWithContext(ctx, "POST", c.endpoint(method), bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *tgClient) sendPhoto(chat int64, png []byte, caption string) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chat_id", strconv.FormatInt(chat, 10))
	_ = w.WriteField("caption", caption)
	_ = w.WriteField("parse_mode", "HTML")
	fw, _ := w.CreateFormFile("photo", "week.png")
	_, _ = fw.Write(png)
	_ = w.Close()
	req, _ := http.NewRequest("POST", c.endpoint("sendPhoto"), &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return c.do(req, nil)
}

func (c *tgClient) send(chat int64, text string, keyboard [][]tgButton) error {
	p := map[string]any{"chat_id": chat, "text": text, "parse_mode": "HTML", "disable_web_page_preview": true}
	if len(keyboard) > 0 {
		p["reply_markup"] = map[string]any{"inline_keyboard": keyboard}
	}
	return c.call("sendMessage", p, nil)
}

type tgButton struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}

type tgUpdate struct {
	ID      int64 `json:"update_id"`
	Message *struct {
		Text string `json:"text"`
		Chat struct {
			ID    int64  `json:"id"`
			Type  string `json:"type"`
			Title string `json:"title"`
			First string `json:"first_name"`
			Last  string `json:"last_name"`
			User  string `json:"username"`
		} `json:"chat"`
	} `json:"message"`
	Callback *struct {
		ID      string `json:"id"`
		Data    string `json:"data"`
		Message *struct {
			Chat struct {
				ID   int64  `json:"id"`
				Type string `json:"type"`
			} `json:"chat"`
		} `json:"message"`
	} `json:"callback_query"`
}

// ---------- polling ----------

func (a *App) setTGStatus(botName, errMsg string, ok bool) {
	s := &a.tg
	s.mu.Lock()
	defer s.mu.Unlock()
	if botName != "" {
		s.botName = botName
	}
	s.lastErr = errMsg
	if ok {
		s.lastOK = time.Now()
	}
}

func (a *App) telegramLoop(ctx context.Context, token string) {
	c := newTG(token, 45*time.Second)
	var me struct {
		User string `json:"username"`
	}
	backoff := 2 * time.Second
	sleep := func() bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
		return true
	}
	for ctx.Err() == nil {
		if err := c.call("getMe", nil, &me); err != nil {
			a.setTGStatus("", err.Error(), false)
			if !sleep() {
				return
			}
			continue
		}
		break
	}
	a.setTGStatus(me.User, "", true)
	backoff = 2 * time.Second
	var offset int64
	for ctx.Err() == nil {
		var ups []tgUpdate
		err := c.callCtx(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 30, "allowed_updates": []string{"message", "callback_query"}}, &ups)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			a.setTGStatus("", err.Error(), false)
			if !sleep() {
				return
			}
			continue
		}
		backoff = 2 * time.Second
		a.setTGStatus("", "", true)
		for _, u := range ups {
			if u.ID >= offset {
				offset = u.ID + 1
			}
			go a.telegramHandle(c, u)
		}
	}
}

// ---------- handling ----------

func (a *App) telegramHandle(c *tgClient, u tgUpdate) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("telegram: handler panic: %v", r)
		}
	}()
	switch {
	case u.Message != nil:
		m := u.Message
		name := strings.TrimSpace(m.Chat.First + " " + m.Chat.Last)
		if name == "" {
			name = m.Chat.Title
		}
		if m.Chat.User != "" {
			name += " (@" + m.Chat.User + ")"
		}
		if !a.tgAuthorised(m.Chat.ID, m.Chat.Type, name) {
			_ = c.send(m.Chat.ID, "This bot is private. To use it, approve this chat on the Holdings <b>Settings</b> page (chat id <code>"+
				strconv.FormatInt(m.Chat.ID, 10)+"</code>).", nil)
			return
		}
		a.tgCommand(c, m.Chat.ID, m.Text)
	case u.Callback != nil && u.Callback.Message != nil:
		cb := u.Callback
		chat := cb.Message.Chat.ID
		if !a.tgAuthorised(chat, cb.Message.Chat.Type, "") {
			return
		}
		_ = c.call("answerCallbackQuery", map[string]any{"callback_query_id": cb.ID}, nil)
		if sym, ok := strings.CutPrefix(cb.Data, "g:"); ok {
			a.tgGraphs(c, chat, sym)
		}
	}
}

// tgAuthorised reports whether chat is the approved one. Otherwise (and only for private chats) it records the chat
// as pending so the Settings page can offer to approve it.
func (a *App) tgAuthorised(chat int64, kind, name string) bool {
	s := &a.tg
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.ChatID != 0 && chat == s.cfg.ChatID {
		return true
	}
	if kind == "private" && name != "" && s.cfg.ChatID == 0 && (s.cfg.PendingChat != chat || s.cfg.PendingName != name) {
		s.cfg.PendingChat, s.cfg.PendingName = chat, name
		_ = s.saveLocked()
	}
	return false
}

func (a *App) tgCommand(c *tgClient, chat int64, text string) {
	f := strings.Fields(text)
	if len(f) == 0 {
		return
	}
	cmd := strings.ToLower(strings.SplitN(f[0], "@", 2)[0])
	arg := ""
	if len(f) > 1 {
		arg = strings.ToUpper(strings.Join(f[1:], " "))
	}
	switch cmd {
	case "/start", "/help":
		_ = c.send(chat, tgHelp, nil)
	case "/prices", "/snapshot", "/status":
		a.tgSnapshot(c, chat, "")
	case "/refresh", "/update", "/pull":
		_ = c.send(chat, "Pulling fresh prices…", nil)
		msg, err := a.RefreshNow()
		note := "✅ " + html.EscapeString(msg)
		if err != nil {
			note = "⚠️ " + html.EscapeString(msg) + "\n" + html.EscapeString(shorten(err.Error(), 300))
		}
		a.tgSnapshot(c, chat, note+"\n\n")
	case "/graph", "/graphs", "/chart", "/week":
		if arg == "" {
			arg = "ALL"
		}
		a.tgGraphs(c, chat, arg)
	default:
		_ = c.send(chat, "I don't know that one.\n\n"+tgHelp, nil)
	}
}

const tgHelp = "<b>Holdings bot</b>\n" +
	"/refresh – pull fresh prices, then send a snapshot\n" +
	"/prices – snapshot of the latest prices\n" +
	"/graph – last week's price graphs for every holding\n" +
	"/graph VGS – the graph for one holding\n" +
	"/graph Super – the graphs for one group"

// ---------- snapshot ----------

// weekPoint is one saved daily price.
type weekPoint struct {
	Date  string
	Value float64
}

// weekSeries returns a holding's prices over the last seven days (falling back to its last few saved prices when the
// window is thin, e.g. after a long weekend), newest last. A live quote newer than the saved history is included.
func weekSeries(db *DB, sym string, now time.Time) []weekPoint {
	m := map[string]float64{}
	for d, v := range db.Prices[sym] {
		m[d] = v
	}
	for d, v := range db.Manual[sym] {
		m[d] = v
	}
	if q, ok := db.Quotes[sym]; ok && q.Price > 0 && q.Date != "" {
		last := ""
		for d := range m {
			if d > last {
				last = d
			}
		}
		if q.Date >= last {
			m[q.Date] = q.Price
		}
	}
	all := make([]weekPoint, 0, len(m))
	for d, v := range m {
		if v > 0 {
			all = append(all, weekPoint{d, v})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Date < all[j].Date })
	from := now.AddDate(0, 0, -7).Format(dateFmt)
	var out []weekPoint
	for _, p := range all {
		if p.Date >= from {
			out = append(out, p)
		}
	}
	if len(out) < 2 && len(all) >= 2 {
		out = all[len(all)-min(len(all), 5):]
	}
	return out
}

func pctMove(from, to float64) float64 {
	if from == 0 {
		return 0
	}
	return (to/from - 1) * 100
}

func arrow(v float64) string {
	switch {
	case v > 0.005:
		return "🟢"
	case v < -0.005:
		return "🔴"
	}
	return "⚪"
}

// tgSnapshot sends the latest prices, with buttons offering the week's graphs.
func (a *App) tgSnapshot(c *tgClient, chat int64, intro string) {
	now := liveNow()
	var sb strings.Builder
	sb.WriteString(intro)
	var syms, groups []string
	test := a.inTest()
	a.st().View(func(db *DB) {
		res := Compute(db, now)
		if test {
			sb.WriteString("<i>Test mode: simulated data</i>\n")
		}
		sb.WriteString("<b>Prices</b>")
		if t, err := time.Parse(time.RFC3339, db.LastRefresh); err == nil {
			sb.WriteString(" · history saved " + t.Local().Format("2 Jan 15:04"))
		}
		sb.WriteString("\n")
		if len(res.Holdings) == 0 {
			sb.WriteString("No open holdings yet.")
			return
		}
		type line struct {
			h      *Holding
			day    float64
			hasDay bool
		}
		var lines []line
		for _, h := range res.Holdings {
			if h.Private {
				continue
			}
			l := line{h: h}
			if pts := weekSeries(db, h.Symbol, now); len(pts) >= 2 {
				n := len(pts)
				l.day, l.hasDay = pctMove(pts[n-2].Value, pts[n-1].Value), true
			}
			lines = append(lines, l)
			syms = append(syms, h.Symbol)
		}
		writeLine := func(l line) {
			h := l.h
			day := ""
			if l.hasDay {
				day = fmt.Sprintf(" %s %+.2f%%", arrow(l.day), l.day)
			}
			ccy := h.Currency
			if ccy == "" {
				ccy = "AUD"
			}
			fmt.Fprintf(&sb, "\n<b>%s</b> %s %s%s", html.EscapeString(short(h.Symbol)), ccy, commas(h.Price, 4), day)
			if h.PriceDate != "" {
				fmt.Fprintf(&sb, " <i>%s</i>", h.PriceDate)
			}
		}
		byGroup := map[string][]line{}
		for _, l := range lines {
			g := db.groupOf(l.h.Symbol)
			byGroup[g] = append(byGroup[g], l)
		}
		if _, plain := byGroup[""]; plain && len(byGroup) == 1 { // no groups set up: the plain list, as before
			for _, l := range lines {
				writeLine(l)
			}
		} else {
			// Each group gets a heading with its value and its value-weighted move since the previous saved price.
			for _, g := range sortedGroups(byGroup) {
				var value, wsum, wday float64
				for _, l := range byGroup[g] {
					value += l.h.ValueAUD
					if l.hasDay && l.h.ValueAUD > 0 {
						wsum += l.h.ValueAUD
						wday += l.h.ValueAUD * l.day
					}
				}
				fmt.Fprintf(&sb, "\n📁 <b>%s</b> · A$%s", html.EscapeString(groupLabel(g)), commas(value, 0))
				if wsum > 0 {
					d := wday / wsum
					fmt.Fprintf(&sb, " %s %+.2f%%", arrow(d), d)
				}
				for _, l := range byGroup[g] {
					writeLine(l)
				}
				sb.WriteString("\n")
				if g != "" {
					groups = append(groups, g)
				}
			}
		}
		if res.Total != nil {
			fmt.Fprintf(&sb, "\n\n<b>Portfolio</b> A$%s", commas(res.Total.ValueAUD, 0))
		}
		if db.RefreshErr != "" {
			sb.WriteString("\n⚠️ " + html.EscapeString(shorten(db.RefreshErr, 200)))
		}
	})
	var kb [][]tgButton
	if len(syms) > 0 {
		sb.WriteString("\n\n📈 Want small graphs of the last week?")
		kb = append(kb, []tgButton{{"📈 All holdings", "g:ALL"}})
		var row []tgButton
		for _, g := range groups {
			row = append(row, tgButton{"📁 " + g, "g:" + g})
			if len(row) == 2 {
				kb, row = append(kb, row), nil
			}
		}
		if len(row) > 0 {
			kb, row = append(kb, row), nil
		}
		for _, s := range syms {
			row = append(row, tgButton{short(s), "g:" + s})
			if len(row) == 3 {
				kb, row = append(kb, row), nil
			}
		}
		if len(row) > 0 {
			kb = append(kb, row)
		}
	}
	if err := c.send(chat, sb.String(), kb); err != nil {
		a.setTGStatus("", err.Error(), false)
	}
}

// ---------- graphs ----------

const maxGraphs = 10

// ---------- groups ----------

// maxGroupLen keeps a group name short enough for a button's callback data (Telegram allows 64 bytes).
const maxGroupLen = 40

// cleanGroup tidies a group name typed on the Settings page.
func cleanGroup(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	for len(s) > maxGroupLen {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	return strings.TrimSpace(s)
}

// groupOf is the Telegram group a holding belongs to ("" = none).
func (d *DB) groupOf(sym string) string {
	if in := d.Instruments[sym]; in != nil {
		return in.Group
	}
	return ""
}

// groupNames lists the distinct groups in use, sorted.
func (d *DB) groupNames() []string {
	set := map[string]bool{}
	for _, in := range d.Instruments {
		if in.Group != "" {
			set[in.Group] = true
		}
	}
	out := make([]string, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

// sortedGroups orders group names alphabetically, with the ungrouped holdings ("") last.
func sortedGroups[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for g := range m {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i] == "") != (out[j] == "") {
			return out[j] == ""
		}
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})
	return out
}

func groupLabel(g string) string {
	if g == "" {
		return "Other"
	}
	return g
}

// tgGraphs sends one small week graph per holding: all of them, the one named (VGS and VGS.AX both work), or every
// holding in the named group.
func (a *App) tgGraphs(c *tgClient, chat int64, which string) {
	now := liveNow()
	type item struct {
		sym, ccy string
		pts      []weekPoint
	}
	var items []item
	a.st().View(func(db *DB) {
		res := Compute(db, now)
		for _, h := range res.Holdings {
			if h.Private {
				continue
			}
			if which != "ALL" && !strings.EqualFold(h.Symbol, which) && !strings.EqualFold(short(h.Symbol), which) &&
				!(db.groupOf(h.Symbol) != "" && strings.EqualFold(db.groupOf(h.Symbol), which)) {
				continue
			}
			ccy := h.Currency
			if ccy == "" {
				ccy = "AUD"
			}
			items = append(items, item{h.Symbol, ccy, weekSeries(db, h.Symbol, now)})
		}
	})
	if len(items) == 0 {
		_ = c.send(chat, "I couldn't find a holding called <b>"+html.EscapeString(which)+"</b>.", nil)
		return
	}
	extra := 0
	if len(items) > maxGraphs {
		extra, items = len(items)-maxGraphs, items[:maxGraphs]
	}
	for _, it := range items {
		if len(it.pts) < 2 {
			_ = c.send(chat, "<b>"+html.EscapeString(short(it.sym))+"</b>: not enough saved prices to draw a week yet.", nil)
			continue
		}
		vals := make([]float64, len(it.pts))
		for i, p := range it.pts {
			vals[i] = p.Value
		}
		first, last := vals[0], vals[len(vals)-1]
		ch := pctMove(first, last)
		lo, hi := vals[0], vals[0]
		for _, v := range vals {
			lo, hi = min(lo, v), max(hi, v)
		}
		caption := fmt.Sprintf("<b>%s</b> %s %s  %s %+.2f%%\n%s → %s · low %s · high %s",
			html.EscapeString(short(it.sym)), it.ccy, commas(last, 4), arrow(ch), ch,
			it.pts[0].Date, it.pts[len(it.pts)-1].Date, commas(lo, 4), commas(hi, 4))
		png, err := sparklinePNG(vals, ch >= 0)
		if err == nil {
			err = c.sendPhoto(chat, png, caption)
		}
		if err != nil {
			a.setTGStatus("", err.Error(), false)
			_ = c.send(chat, "Couldn't send the graph for "+html.EscapeString(short(it.sym))+".", nil)
			return
		}
	}
	if extra > 0 {
		_ = c.send(chat, fmt.Sprintf("Showing the first %d; ask for the other %d one at a time, e.g. <code>/graph VGS</code>.", maxGraphs, extra), nil)
	}
}

// tgSendTest sends a short message to the approved chat, for the Settings page's "Send test message" button.
func (a *App) tgSendTest() error {
	s := &a.tg
	s.mu.Lock()
	token, chat := s.cfg.Token, s.cfg.ChatID
	s.mu.Unlock()
	if token == "" {
		return errors.New("save a bot token first")
	}
	if chat == 0 {
		return errors.New("approve a chat first")
	}
	return newTG(token, 20*time.Second).send(chat, "✅ Holdings is connected. Send /help to see what I can do.", nil)
}

// tgCheckToken confirms a token with Telegram and returns the bot's username.
func tgCheckToken(token string) (string, error) {
	var me struct {
		User string `json:"username"`
	}
	if err := newTG(token, 15*time.Second).call("getMe", nil, &me); err != nil {
		return "", err
	}
	return me.User, nil
}

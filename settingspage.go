package main

import (
	"net/http"
	"strconv"
	"strings"
)

func (a *App) registerSettings(mux *http.ServeMux) {
	mux.HandleFunc("GET /settings", a.settingsPage)
	mux.HandleFunc("POST /settings/telegram", a.saveTelegram)
	mux.HandleFunc("POST /settings/telegram/approve", a.approveTelegram)
	mux.HandleFunc("POST /settings/telegram/test", a.testTelegram)
	mux.HandleFunc("POST /settings/telegram/forget", a.forgetTelegram)
}

func (a *App) settingsPage(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "settings", map[string]any{"TG": a.telegramView()})
}

func (a *App) saveTelegram(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	token := strings.TrimSpace(r.FormValue("token"))
	enabled := r.FormValue("enabled") != ""
	s := &a.tg
	s.mu.Lock()
	cur := s.cfg.Token
	s.mu.Unlock()
	if token == "" {
		token = cur // blank keeps the saved token
	}
	if enabled && token == "" {
		redirect(w, r, "/settings", "", "Enter the bot token from BotFather to turn the bot on.")
		return
	}
	var name string
	if token != "" && token != cur {
		var err error
		if name, err = tgCheckToken(token); err != nil {
			redirect(w, r, "/settings", "", "Telegram didn't accept that token: "+err.Error())
			return
		}
	}
	chat := int64(0)
	if v := strings.TrimSpace(r.FormValue("chat_id")); v != "" {
		var err error
		if chat, err = strconv.ParseInt(v, 10, 64); err != nil {
			redirect(w, r, "/settings", "", "The chat id should be a number, e.g. 123456789.")
			return
		}
	}
	s.mu.Lock()
	if token != cur { // a different bot: a pending request from the old one doesn't carry over
		s.cfg.PendingChat, s.cfg.PendingName = 0, ""
	}
	s.cfg.Token, s.cfg.Enabled, s.cfg.ChatID = token, enabled, chat
	if chat != 0 {
		s.cfg.PendingChat, s.cfg.PendingName = 0, ""
	}
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		redirect(w, r, "/settings", "", "Couldn't save: "+err.Error())
		return
	}
	if name != "" {
		a.setTGStatus(name, "", false)
	}
	a.startTelegram()
	msg := "Telegram settings saved."
	switch {
	case !enabled:
		msg += " The bot is off."
	case chat == 0:
		msg += " Now send /start to your bot in Telegram, then approve the chat below."
	}
	redirect(w, r, "/settings", msg, "")
}

func (a *App) approveTelegram(w http.ResponseWriter, r *http.Request) {
	s := &a.tg
	s.mu.Lock()
	// the form carries the chat id being approved so a newer request can't be approved by mistake
	id, _ := strconv.ParseInt(r.FormValue("chat"), 10, 64)
	ok := id != 0 && id == s.cfg.PendingChat
	if ok {
		s.cfg.ChatID, s.cfg.PendingChat, s.cfg.PendingName = id, 0, ""
		_ = s.saveLocked()
	}
	s.mu.Unlock()
	if !ok {
		redirect(w, r, "/settings", "", "That request is no longer pending. Message the bot again and retry.")
		return
	}
	redirect(w, r, "/settings", "Chat approved. Only this chat can use the bot.", "")
}

func (a *App) testTelegram(w http.ResponseWriter, r *http.Request) {
	if err := a.tgSendTest(); err != nil {
		redirect(w, r, "/settings", "", "Test message failed: "+err.Error())
		return
	}
	redirect(w, r, "/settings", "Test message sent.", "")
}

func (a *App) forgetTelegram(w http.ResponseWriter, r *http.Request) {
	s := &a.tg
	s.mu.Lock()
	s.cfg = TelegramConfig{}
	err := s.saveLocked()
	s.mu.Unlock()
	a.startTelegram()
	a.setTGStatus("", "", false)
	if err != nil {
		redirect(w, r, "/settings", "", err.Error())
		return
	}
	redirect(w, r, "/settings", "Telegram settings removed.", "")
}

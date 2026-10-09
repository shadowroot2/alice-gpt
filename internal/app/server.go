package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
)

type Request struct {
	Version string `json:"version"`
	Request struct {
		Type     string `json:"type"`
		Command  string `json:"command"`
		Original string `json:"original_utterance"`
	} `json:"request"`
	Session struct {
		ID        string `json:"session_id"`
		SkillID   string `json:"skill_id"`
		MessageID int    `json:"message_id"`
		New       bool   `json:"new"`
		User      struct {
			ID string `json:"user_id"`
		} `json:"user"`
		Application struct {
			ID string `json:"application_id"`
		} `json:"application"`
	} `json:"session"`
	State struct {
		Session map[string]json.RawMessage `json:"session"`
	} `json:"state"`
}
type Reply struct {
	Version  string `json:"version"`
	Response struct {
		Text       string `json:"text"`
		EndSession bool   `json:"end_session"`
	} `json:"response"`
	SessionState map[string]json.RawMessage `json:"session_state"`
}
type Server struct {
	Config Config
	Store  Store
	GPT    Generator
	Log    *slog.Logger
	slots  chan struct{}
}

func NewServer(c Config, s Store, g Generator, l *slog.Logger) *Server {
	return &Server{c, s, g, l, make(chan struct{}, c.MaxConcurrent)}
}
func (s *Server) Handler() http.Handler {
	router := chi.NewRouter()
	router.Get("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	router.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 300*time.Millisecond)
		defer cancel()
		if s.Store.Ping(ctx) != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		w.WriteHeader(200)
	})
	router.Post("/api", s.api)
	return router
}
func digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	supplied := digest(r.URL.Query().Get("token"))
	expected := digest(s.Config.WebhookToken)
	if subtle.ConstantTimeCompare([]byte(supplied), []byte(expected)) != 1 {
		http.Error(w, "forbidden", 403)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3700*time.Millisecond)
	defer cancel()
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req Request
	dec := json.NewDecoder(r.Body)
	if dec.Decode(&req) != nil || dec.Decode(new(any)) != io.EOF || req.Version != "1.0" || req.Session.ID == "" || len(req.Session.ID) > 64 || req.Session.MessageID < 0 || req.Session.MessageID > 99999999 {
		http.Error(w, "invalid request", 400)
		return
	}
	if req.Session.SkillID != s.Config.SkillID {
		http.Error(w, "forbidden", 403)
		return
	}
	stateBytes, _ := json.Marshal(req.State.Session)
	if len(stateBytes) > 900 {
		http.Error(w, "session state too large", 400)
		return
	}
	state := req.State.Session
	if state == nil {
		state = map[string]json.RawMessage{}
	}
	respond := func(text string, end bool) { reply := newReply(text, end, state); writeReply(w, reply) }
	if req.Request.Type != "SimpleUtterance" {
		respond("Пожалуйста, задайте вопрос голосом.", false)
		return
	}
	identity := req.Session.User.ID
	if identity == "" {
		identity = req.Session.Application.ID
	}
	if identity == "" {
		identity = req.Session.ID
	}
	key := "alice:" + digest(req.Session.SkillID+":"+identity+":"+req.Session.ID)
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		respond("Сервис временно недоступен.", false)
		return
	}
	token := hex.EncodeToString(tokenBytes)
	locked, err := s.Store.Acquire(ctx, key, token)
	if err != nil {
		s.Log.Warn("redis lock failed")
		respond("Сервис временно недоступен. Попробуйте позже.", false)
		return
	}
	if !locked {
		respond("Предыдущий вопрос ещё обрабатывается. Повторите через секунду.", false)
		return
	}
	defer func() {
		c, stop := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer stop()
		s.Store.Release(c, key, token)
	}()
	c, err := s.Store.Load(ctx, key)
	if err != nil {
		s.Log.Warn("redis read failed")
		respond("Не удалось прочитать историю. Попробуйте позже.", false)
		return
	}
	if c.Reply != nil && req.Session.MessageID == c.LastID {
		writeReply(w, *c.Reply)
		return
	}
	if req.Session.MessageID < c.LastID {
		respond("Этот вопрос уже обработан. Задайте следующий.", false)
		return
	}
	allowed, err := s.Store.Allow(ctx, "alice:limit:"+digest(req.Session.SkillID+":"+identity), s.Config.RequestsPerMinute)
	if err != nil || !allowed {
		respond("Слишком много запросов. Подождите минуту.", false)
		return
	}
	text := strings.TrimSpace(req.Request.Command)
	if text == "" && !req.Session.New {
		text = strings.TrimSpace(req.Request.Original)
	}
	command := strings.Trim(strings.ToLower(strings.TrimSpace(req.Request.Command)), " .!?,")
	if command == "" {
		command = strings.Trim(strings.ToLower(text), " .!?,")
	}
	answer := ""
	end := false
	reset := false
	switch command {
	case "выход", "хватит", "стоп", "до свидания":
		answer = "До свидания!"
		end = true
		reset = true
	case "сброс", "сбрось контекст", "начать заново", "новый разговор":
		answer = "Начинаем заново. О чём поговорим?"
		reset = true
	case "помощь", "что ты умеешь":
		answer = "Отвечаю на вопросы и помню наш разговор. Скажите «сброс», чтобы очистить историю, или «выход»."
	default:
		if text == "" {
			answer = "Здравствуйте! Задайте вопрос."
		} else if utf8.RuneCountInString(text) > 2000 {
			answer = "Вопрос слишком длинный. Сформулируйте короче."
		} else {
			select {
			case s.slots <- struct{}{}:
			default:
				respond("Сервис занят. Попробуйте через несколько секунд.", false)
				return
			}
			gctx, stop := context.WithTimeout(ctx, s.Config.GPTTimeout)
			generated, e := s.GPT.Generate(gctx, append(append([]Message{}, c.Messages...), Message{"user", text}))
			timedOut := errors.Is(gctx.Err(), context.DeadlineExceeded)
			stop()
			<-s.slots
			if e != nil {
				s.Log.Warn("generation failed", "timeout", timedOut, "error", e)
				message := "Не удалось получить ответ. Повторите вопрос."
				if timedOut {
					message = "Не удалось получить ответ вовремя. Повторите вопрос."
				}
				respond(message, false)
				return
			}
			answer = voiceText(generated, s.Config.MaxAnswerChars)
			if answer == "" {
				respond("Не удалось получить ответ. Повторите вопрос.", false)
				return
			}
			c.Messages = append(c.Messages, Message{"user", text}, Message{"assistant", answer})
			if n := len(c.Messages) - s.Config.HistoryTurns*2; n > 0 {
				c.Messages = c.Messages[n:]
			}
			c.Turns++
		}
	}
	if reset {
		c.Messages = nil
		c.Turns = 0
		state = map[string]json.RawMessage{}
	}
	state["turns"], _ = json.Marshal(c.Turns)
	state["last_message_id"], _ = json.Marshal(req.Session.MessageID)
	reply := newReply(answer, end, state)
	c.LastID = req.Session.MessageID
	c.Reply = &reply
	if err := s.Store.Save(ctx, key, c); err != nil {
		s.Log.Warn("redis write failed")
		respond("Не удалось сохранить разговор. Повторите вопрос.", false)
		return
	}
	writeReply(w, reply)
}
func newReply(text string, end bool, state map[string]json.RawMessage) Reply {
	r := Reply{Version: "1.0", SessionState: state}
	r.Response.Text = text
	r.Response.EndSession = end
	return r
}
func writeReply(w http.ResponseWriter, r Reply) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(r)
}

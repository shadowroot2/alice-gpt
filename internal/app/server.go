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
	"sync"
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
	jobs   sync.WaitGroup
	mu     sync.Mutex
	active map[string]generation
}

type generation struct {
	token  string
	cancel context.CancelFunc
}

const (
	thinkingText = "Думаю... чтобы продолжить скажи продолжить"
	waitingText  = "Все еще думаю.... чтобы продолжить скажи продолжить"
)

func NewServer(c Config, s Store, g Generator, l *slog.Logger) *Server {
	return &Server{Config: c, Store: s, GPT: g, Log: l, slots: make(chan struct{}, c.MaxConcurrent), active: make(map[string]generation)}
}

func (s *Server) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { s.jobs.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
	text := strings.TrimSpace(req.Request.Command)
	if text == "" && !req.Session.New {
		text = strings.TrimSpace(req.Request.Original)
	}
	command := strings.Trim(strings.ToLower(text), " .!?,")
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
		current, loadErr := s.Store.Load(ctx, key)
		if loadErr != nil {
			s.Log.Warn("redis read failed")
			respond("Не удалось прочитать историю. Попробуйте позже.", false)
			return
		}
		if current.Reply != nil && current.LastID == req.Session.MessageID {
			if current.Pending != nil && current.Pending.Done {
				ackCtx, cancel := context.WithTimeout(ctx, 350*time.Millisecond)
				if reply, ok := s.ackInitial(ackCtx, key, token+":ack", req.Session.MessageID); ok {
					current.Reply = &reply
				}
				cancel()
			}
			writeReply(w, *current.Reply)
			return
		}
		if command == "продолжить" && current.Pending != nil {
			writeReply(w, s.awaitPending(ctx, key, token, req.Session.MessageID, state))
			return
		}
		if current.Pending != nil && !current.Pending.Done {
			s.cancelGeneration(key)
		}
		waitCtx, stop := context.WithTimeout(ctx, 700*time.Millisecond)
		locked, err = s.acquire(waitCtx, key, token)
		stop()
		if err != nil || !locked {
			respond("Предыдущий запрос ещё отменяется. Повторите фразу.", false)
			return
		}
	}
	owned := true
	defer func() {
		if owned {
			s.release(key, token)
		}
	}()
	c, err := s.Store.Load(ctx, key)
	if err != nil {
		s.Log.Warn("redis read failed")
		respond("Не удалось прочитать историю. Попробуйте позже.", false)
		return
	}
	if c.Reply != nil && req.Session.MessageID == c.LastID {
		if c.Pending != nil && c.Pending.Done {
			reply, _ := s.deliverPending(ctx, key, c, req.Session.MessageID, state)
			writeReply(w, reply)
			return
		}
		writeReply(w, *c.Reply)
		return
	}
	if req.Session.MessageID < c.LastID {
		respond("Этот вопрос уже обработан. Задайте следующий.", false)
		return
	}
	if c.Pending != nil {
		if command == "продолжить" {
			if c.Pending.Done {
				reply, _ := s.deliverPending(ctx, key, c, req.Session.MessageID, state)
				writeReply(w, reply)
				return
			}
			if time.Since(c.Pending.StartedAt) > pendingLockTTL {
				respond("Ответ не получен. Повторите вопрос.", false)
				return
			}
			s.release(key, token)
			owned = false
			writeReply(w, s.awaitPending(ctx, key, token, req.Session.MessageID, state))
			return
		}
		s.cancelGeneration(key)
		c.Pending = nil
		if err := s.Store.Save(ctx, key, c); err != nil {
			s.Log.Warn("redis write failed")
			respond("Не удалось сохранить разговор. Повторите фразу.", false)
			return
		}
	}
	allowed, err := s.Store.Allow(ctx, "alice:limit:"+digest(req.Session.SkillID+":"+identity), s.Config.RequestsPerMinute)
	if err != nil || !allowed {
		respond("Слишком много запросов. Подождите минуту.", false)
		return
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
		answer = "Отвечаю на вопросы и помню разговор. Если я думаю, скажите «продолжить». Для очистки истории скажите «сброс»."
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
			state["turns"], _ = json.Marshal(c.Turns)
			state["last_message_id"], _ = json.Marshal(req.Session.MessageID)
			thinking := newReply(thinkingText, false, cloneState(state))
			c.LastID = req.Session.MessageID
			c.Reply = &thinking
			c.Pending = &Pending{StartedAt: time.Now().UTC(), Question: text}
			if err := s.Store.Save(ctx, key, c); err != nil {
				<-s.slots
				s.Log.Warn("redis write failed")
				respond("Не удалось сохранить разговор. Повторите вопрос.", false)
				return
			}
			done := make(chan bool, 1)
			jobCtx, jobCancel := context.WithTimeout(context.Background(), s.Config.GPTBackgroundTimeout)
			s.mu.Lock()
			s.active[key] = generation{token: token, cancel: jobCancel}
			s.mu.Unlock()
			s.jobs.Add(1)
			owned = false
			go s.generate(jobCtx, jobCancel, key, token, c, text, done)
			waitCtx, stop := context.WithTimeout(ctx, s.Config.GPTTimeout)
			defer stop()
			select {
			case saved := <-done:
				if !saved {
					respond("Запрос отменён или ответ не сохранён. Повторите вопрос.", false)
					return
				}
				ackCtx, cancel := context.WithTimeout(ctx, 350*time.Millisecond)
				reply, ok := s.ackInitial(ackCtx, key, token+":ack", req.Session.MessageID)
				cancel()
				if ok {
					writeReply(w, reply)
				} else {
					writeReply(w, thinking)
				}
			case <-waitCtx.Done():
				writeReply(w, thinking)
			}
			return
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

func (s *Server) release(key, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.Store.Release(ctx, key, token)
}

func (s *Server) acquire(ctx context.Context, key, token string) (bool, error) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		locked, err := s.Store.Acquire(ctx, key, token)
		if locked || err != nil {
			return locked, err
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Server) cancelGeneration(key string) {
	s.mu.Lock()
	active, ok := s.active[key]
	s.mu.Unlock()
	if ok {
		active.cancel()
	}
}

func (s *Server) unregister(key, token string) {
	s.mu.Lock()
	if s.active[key].token == token {
		delete(s.active, key)
	}
	s.mu.Unlock()
}

func (s *Server) generate(ctx context.Context, cancel context.CancelFunc, key, token string, c Conversation, question string, done chan<- bool) {
	defer s.jobs.Done()
	defer s.release(key, token)
	defer func() { <-s.slots }()
	defer s.unregister(key, token)
	defer cancel()

	input := append(append([]Message{}, c.Messages...), Message{Role: "user", Content: question})
	generated, err := s.GPT.Generate(ctx, input)
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	if errors.Is(ctx.Err(), context.Canceled) {
		done <- false
		return
	}
	answer := voiceText(generated, s.Config.MaxAnswerChars)
	success := err == nil && !timedOut && answer != ""
	if !success {
		if err != nil {
			s.Log.Warn("generation failed", "timeout", timedOut, "error", err)
		} else {
			s.Log.Warn("generation returned empty response")
		}
		answer = "Не удалось получить ответ. Повторите вопрос."
	}
	c.Pending = &Pending{StartedAt: c.Pending.StartedAt, Question: question, Answer: answer, Done: true, Success: success}
	if errors.Is(ctx.Err(), context.Canceled) {
		done <- false
		return
	}
	saveCtx, stop := context.WithTimeout(context.Background(), time.Second)
	err = s.Store.Save(saveCtx, key, c)
	stop()
	if err != nil {
		s.Log.Warn("redis write failed after generation", "error", err)
		done <- false
		return
	}
	done <- true
}

func (s *Server) ackInitial(ctx context.Context, key, token string, messageID int) (Reply, bool) {
	locked, err := s.acquire(ctx, key, token)
	if err != nil || !locked {
		return Reply{}, false
	}
	defer s.release(key, token)
	c, err := s.Store.Load(ctx, key)
	if err != nil {
		s.Log.Warn("redis read failed")
		return Reply{}, false
	}
	if c.LastID != messageID || c.Pending == nil || !c.Pending.Done {
		return Reply{}, false
	}
	return s.deliverPending(ctx, key, c, messageID, c.Reply.SessionState)
}

func (s *Server) awaitPending(ctx context.Context, key, token string, messageID int, state map[string]json.RawMessage) Reply {
	waitCtx, cancel := context.WithTimeout(ctx, s.Config.GPTTimeout)
	defer cancel()
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	for {
		c, err := s.Store.Load(waitCtx, key)
		if err != nil {
			s.Log.Warn("redis read failed")
			return newReply("Не удалось прочитать ответ. Повторите позже.", false, state)
		}
		if c.Reply != nil && c.LastID == messageID {
			return *c.Reply
		}
		if c.Pending == nil {
			return newReply("Ответ уже получен. Повторите команду.", false, state)
		}
		if c.Pending.Done {
			locked, err := s.Store.Acquire(waitCtx, key, token)
			if err != nil {
				s.Log.Warn("redis lock failed")
				return newReply("Сервис временно недоступен. Попробуйте позже.", false, state)
			}
			if locked {
				current, loadErr := s.Store.Load(waitCtx, key)
				var reply Reply
				switch {
				case loadErr != nil:
					reply = newReply("Не удалось прочитать ответ. Повторите позже.", false, state)
				case current.Reply != nil && current.LastID == messageID:
					reply = *current.Reply
				case current.Pending != nil && current.Pending.Done:
					reply, _ = s.deliverPending(waitCtx, key, current, messageID, state)
				default:
					reply = newReply("Ответ уже получен. Задайте вопрос.", false, state)
				}
				s.release(key, token)
				return reply
			}
		}
		if time.Since(c.Pending.StartedAt) > pendingLockTTL {
			return newReply("Ответ не получен. Повторите вопрос.", false, state)
		}
		select {
		case <-waitCtx.Done():
			return newReply(waitingText, false, state)
		case <-ticker.C:
		}
	}
}

func (s *Server) deliverPending(ctx context.Context, key string, c Conversation, messageID int, state map[string]json.RawMessage) (Reply, bool) {
	state = cloneState(state)
	if c.Pending.Success {
		c.Messages = append(c.Messages, Message{Role: "user", Content: c.Pending.Question}, Message{Role: "assistant", Content: c.Pending.Answer})
		if n := len(c.Messages) - s.Config.HistoryTurns*2; n > 0 {
			c.Messages = c.Messages[n:]
		}
		c.Turns++
	}
	state["turns"], _ = json.Marshal(c.Turns)
	state["last_message_id"], _ = json.Marshal(messageID)
	reply := newReply(c.Pending.Answer, false, state)
	c.Pending = nil
	c.LastID = messageID
	c.Reply = &reply
	if err := s.Store.Save(ctx, key, c); err != nil {
		s.Log.Warn("redis write failed")
		return newReply("Не удалось сохранить ответ. Повторите вопрос.", false, state), false
	}
	return reply, true
}

func cloneState(state map[string]json.RawMessage) map[string]json.RawMessage {
	copy := make(map[string]json.RawMessage, len(state))
	for key, value := range state {
		copy[key] = value
	}
	return copy
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

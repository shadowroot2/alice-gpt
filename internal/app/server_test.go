package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type memoryStore struct {
	sync.Mutex
	data   map[string]Conversation
	locked map[string]string
	failed bool
}

func (m *memoryStore) Ping(context.Context) error { return nil }
func (m *memoryStore) Acquire(_ context.Context, k, t string) (bool, error) {
	m.Lock()
	defer m.Unlock()
	if m.locked[k] != "" {
		return false, nil
	}
	m.locked[k] = t
	return true, nil
}
func (m *memoryStore) Release(_ context.Context, k, t string) {
	m.Lock()
	defer m.Unlock()
	if m.locked[k] == t {
		delete(m.locked, k)
	}
}
func (m *memoryStore) Load(_ context.Context, k string) (Conversation, error) {
	m.Lock()
	defer m.Unlock()
	if m.failed {
		return Conversation{}, errors.New("offline")
	}
	c, ok := m.data[k]
	if !ok {
		c.LastID = -1
	}
	return c, nil
}
func (m *memoryStore) Save(_ context.Context, k string, c Conversation) error {
	m.Lock()
	defer m.Unlock()
	m.data[k] = c
	return nil
}
func (m *memoryStore) Allow(context.Context, string, int) (bool, error) { return true, nil }

type generatorFunc func(context.Context, []Message) (string, error)

func (f generatorFunc) Generate(c context.Context, m []Message) (string, error) { return f(c, m) }
func fixture(g Generator) (http.Handler, *memoryStore) {
	m := &memoryStore{data: map[string]Conversation{}, locked: map[string]string{}}
	c := Config{WebhookToken: strings.Repeat("x", 32), SkillID: "skill", GPTTimeout: 50 * time.Millisecond, HistoryTurns: 2, MaxAnswerChars: 100, RequestsPerMinute: 20, MaxConcurrent: 2}
	return NewServer(c, m, g, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler(), m
}
func call(t *testing.T, h http.Handler, id int, text, session string) Reply {
	t.Helper()
	payload := map[string]any{"version": "1.0", "request": map[string]any{"type": "SimpleUtterance", "command": text, "original_utterance": text}, "session": map[string]any{"session_id": session, "skill_id": "skill", "message_id": id, "new": id == 0}, "state": map[string]any{"session": map[string]any{"custom": true}}}
	b, _ := json.Marshal(payload)
	r := httptest.NewRequest("POST", "/api?token="+strings.Repeat("x", 32), strings.NewReader(string(b)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var reply Reply
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}
func TestConversationAndRetries(t *testing.T) {
	var inputs [][]Message
	h, _ := fixture(generatorFunc(func(_ context.Context, m []Message) (string, error) {
		inputs = append(inputs, append([]Message{}, m...))
		return "Короткий ответ.", nil
	}))
	a := call(t, h, 0, "Первый вопрос", "s")
	b := call(t, h, 0, "Первый вопрос", "s")
	if a.Response.Text != b.Response.Text || len(inputs) != 1 {
		t.Fatal("retry generated twice")
	}
	call(t, h, 1, "Второй вопрос", "s")
	if len(inputs[1]) != 3 || inputs[1][0].Content != "Первый вопрос" {
		t.Fatal("history lost")
	}
	if string(a.SessionState["custom"]) != "true" {
		t.Fatal("session state lost")
	}
	for i := 2; i < 6; i++ {
		call(t, h, i, "вопрос", "s")
	}
	if len(inputs[len(inputs)-1]) != 5 {
		t.Fatal("unbounded history")
	}
	call(t, h, 0, "Вопрос другой сессии", "other")
	if len(inputs[len(inputs)-1]) != 1 {
		t.Fatal("session leaked")
	}
}
func TestResetAndExit(t *testing.T) {
	var last []Message
	h, _ := fixture(generatorFunc(func(_ context.Context, m []Message) (string, error) { last = m; return "Ответ", nil }))
	call(t, h, 0, "вопрос", "s")
	r := call(t, h, 1, "сброс", "s")
	if string(r.SessionState["turns"]) != "0" {
		t.Fatal("reset failed")
	}
	call(t, h, 2, "другой вопрос", "s")
	if len(last) != 1 {
		t.Fatal("old context survived")
	}
	if !call(t, h, 3, "выход", "s").Response.EndSession {
		t.Fatal("exit failed")
	}
}
func TestTimeoutDoesNotSave(t *testing.T) {
	h, m := fixture(generatorFunc(func(ctx context.Context, _ []Message) (string, error) { <-ctx.Done(); return "", ctx.Err() }))
	start := time.Now()
	r := call(t, h, 0, "вопрос", "s")
	if !strings.Contains(r.Response.Text, "Повторите") || time.Since(start) > time.Second {
		t.Fatal("timeout fallback failed")
	}
	if len(m.data) != 0 {
		t.Fatal("failed response polluted history")
	}
}
func TestGenerationFailureDoesNotClaimTimeout(t *testing.T) {
	h, m := fixture(generatorFunc(func(context.Context, []Message) (string, error) {
		return "", errors.New("upstream failed")
	}))
	r := call(t, h, 0, "вопрос", "s")
	if r.Response.Text != "Не удалось получить ответ. Повторите вопрос." {
		t.Fatalf("unexpected response: %q", r.Response.Text)
	}
	if len(m.data) != 0 {
		t.Fatal("failed response polluted history")
	}
}
func TestStoreUnavailable(t *testing.T) {
	h, m := fixture(generatorFunc(func(context.Context, []Message) (string, error) {
		t.Fatal("GPT called without history")
		return "", nil
	}))
	m.failed = true
	call(t, h, 0, "вопрос", "s")
}
func TestValidation(t *testing.T) {
	h, _ := fixture(generatorFunc(func(context.Context, []Message) (string, error) { t.Fatal("GPT called"); return "", nil }))
	for _, tc := range []struct {
		url, body string
		status    int
	}{{"/api", "{}", 403}, {"/api?token=" + strings.Repeat("x", 32), "{}", 400}, {"/api?token=" + strings.Repeat("x", 32), strings.Repeat("x", 70<<10), 400}} {
		r := httptest.NewRequest("POST", tc.url, strings.NewReader(tc.body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("got %d want %d", w.Code, tc.status)
		}
	}
}
func TestConcurrentSession(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	h, _ := fixture(generatorFunc(func(context.Context, []Message) (string, error) { close(entered); <-release; return "Ответ", nil }))
	done := make(chan struct{})
	go func() { defer close(done); call(t, h, 0, "первый", "s") }()
	<-entered
	r := call(t, h, 1, "второй", "s")
	if !strings.Contains(r.Response.Text, "обрабатывается") {
		t.Fatal("concurrent request not blocked")
	}
	close(release)
	<-done
}
func TestVoiceText(t *testing.T) {
	for _, s := range []string{strings.Repeat("длинный текст ", 100), "**Привет**\nмир", strings.Repeat("я", 1000)} {
		out := voiceText(s, 100)
		if !utf8.ValidString(out) || utf8.RuneCountInString(out) > 100 || strings.ContainsAny(out, "*\n") {
			t.Fatalf("bad text: %q", out)
		}
	}
}
func TestOpenAI(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("wrong request")
		}
		var body struct {
			Store bool
			Input []Message
			Model string
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Store || body.Model != "test-model" || len(body.Input) != 1 {
			t.Error("wrong body")
		}
		io.WriteString(w, `{"output":[{"type":"message","content":[{"type":"output_text","text":"Привет"}]}]}`)
	}))
	defer upstream.Close()
	o := OpenAI{Key: "test-key", Model: "test-model", URL: upstream.URL + "/v1/responses", Client: upstream.Client()}
	answer, err := o.Generate(context.Background(), []Message{{"user", "вопрос"}})
	if err != nil || answer != "Привет" {
		t.Fatalf("%q %v", answer, err)
	}
}
func TestOpenAIErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{{429, `{"error":"secret details"}`}, {200, `{"output":[]}`}, {200, `invalid`}} {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); io.WriteString(w, tc.body) }))
		o := OpenAI{URL: up.URL, Client: up.Client()}
		_, err := o.Generate(context.Background(), nil)
		up.Close()
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("bad error handling")
		}
	}
}

func TestLaunchDoesNotCallGPT(t *testing.T) {
	h, _ := fixture(generatorFunc(func(context.Context, []Message) (string, error) { t.Fatal("GPT called for launch"); return "", nil }))
	r := call(t, h, 0, "", "s")
	if !strings.Contains(r.Response.Text, "Здравствуйте") {
		t.Fatal("missing greeting")
	}
}

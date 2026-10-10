package app

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	APIKey, Model, RedisURL, SkillID, WebhookToken, Listen         string
	GPTTimeout, GPTBackgroundTimeout, SessionTTL                   time.Duration
	HistoryTurns, MaxAnswerChars, RequestsPerMinute, MaxConcurrent int
}

func LoadConfig() (Config, error) {
	c := Config{APIKey: os.Getenv("GPT_API_KEY"), Model: env("GPT_MODEL", "gpt-4.1-mini"), RedisURL: env("REDIS_URL", "redis://redis:6379/0"), SkillID: os.Getenv("ALICE_SKILL_ID"), WebhookToken: os.Getenv("WEBHOOK_TOKEN"), Listen: env("LISTEN_ADDR", ":8080")}
	if c.APIKey == "" || c.SkillID == "" || len(c.WebhookToken) < 32 {
		return c, fmt.Errorf("GPT_API_KEY, ALICE_SKILL_ID and WEBHOOK_TOKEN (at least 32 characters) are required")
	}
	var err error
	if c.GPTTimeout, err = time.ParseDuration(env("GPT_TIMEOUT", "3s")); err != nil || c.GPTTimeout < 100*time.Millisecond || c.GPTTimeout > 3200*time.Millisecond {
		return c, fmt.Errorf("GPT_TIMEOUT must be between 100ms and 3.2s")
	}
	if c.GPTBackgroundTimeout, err = time.ParseDuration(env("GPT_BACKGROUND_TIMEOUT", "25s")); err != nil || c.GPTBackgroundTimeout < 5*time.Second || c.GPTBackgroundTimeout > 30*time.Second {
		return c, fmt.Errorf("GPT_BACKGROUND_TIMEOUT must be between 5s and 30s")
	}
	if c.SessionTTL, err = time.ParseDuration(env("SESSION_TTL", "24h")); err != nil || c.SessionTTL < time.Minute || c.SessionTTL > 24*time.Hour {
		return c, fmt.Errorf("SESSION_TTL must be between 1m and 24h")
	}
	for _, v := range []struct {
		name          string
		target        *int
		def, min, max int
	}{
		{"HISTORY_TURNS", &c.HistoryTurns, 10, 1, 50}, {"MAX_ANSWER_CHARS", &c.MaxAnswerChars, 500, 50, 1000}, {"REQUESTS_PER_MINUTE", &c.RequestsPerMinute, 20, 1, 1000}, {"MAX_CONCURRENT_GPT", &c.MaxConcurrent, 8, 1, 100},
	} {
		n, e := strconv.Atoi(env(v.name, strconv.Itoa(v.def)))
		if e != nil || n < v.min || n > v.max {
			return c, fmt.Errorf("invalid %s", v.name)
		}
		*v.target = n
	}
	return c, nil
}
func env(k, fallback string) string {
	if s := os.Getenv(k); s != "" {
		return s
	}
	return fallback
}

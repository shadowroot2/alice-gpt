package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type Generator interface {
	Generate(context.Context, []Message) (string, error)
}
type OpenAI struct {
	Key, Model, URL, Instructions string
	Client                        *http.Client
}

func (o *OpenAI) Generate(ctx context.Context, messages []Message) (string, error) {
	request := map[string]any{"model": o.Model, "input": messages, "store": false, "max_output_tokens": 220}
	if o.Instructions != "" {
		request["instructions"] = o.Instructions
	}
	body, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.URL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+o.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("openai transport: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openai status %d", resp.StatusCode)
	}
	var result struct {
		Output []struct {
			Type    string                                 `json:"type"`
			Content []struct{ Type, Text, Refusal string } `json:"content"`
		} `json:"output"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", fmt.Errorf("openai invalid response")
	}
	var parts []string
	for _, item := range result.Output {
		if item.Type != "message" {
			continue
		}
		for _, p := range item.Content {
			if p.Type == "output_text" {
				parts = append(parts, p.Text)
			}
			if p.Type == "refusal" {
				parts = append(parts, p.Refusal)
			}
		}
	}
	answer := strings.TrimSpace(strings.Join(parts, " "))
	if answer == "" {
		return "", fmt.Errorf("openai empty response")
	}
	return answer, nil
}
func voiceText(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		if strings.ContainsRune("*_`#<>[]", r) {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	r = r[:limit-1]
	for i := len(r) - 1; i >= len(r)/2; i-- {
		if strings.ContainsRune(".!?", r[i]) {
			return string(r[:i+1])
		}
	}
	for i := len(r) - 1; i >= len(r)/2; i-- {
		if r[i] == ' ' {
			return strings.TrimRight(string(r[:i]), ",;:") + "…"
		}
	}
	return string(r) + "…"
}

package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// modelHandler is the "model" role: an OpenAI-compatible chat completions
// endpoint for the real decision-waf's OpenAI backend. It answers {"category":
// "sqli"} when the last user message contains "attack" and {"category":
// "benign"} otherwise, as choices[0].message.content, with no logprobs. It
// accepts every request format decision-waf's startup probe tries (json_schema
// and logprobs first), so the probe succeeds on its first attempt, finds no
// logprobs, and settles on JSON-schema requests without confidences. The
// probe's own request is GET / with no "attack" in it, so it gets "benign",
// a valid label. Only the user message is inspected: the system message
// lists the categories and always mentions attacks.
func modelHandler(s *state) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := s.record(r)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var in struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(c.Body, &in); err != nil {
			http.Error(w, `{"error":{"message":"bad request"}}`, http.StatusBadRequest)
			return
		}
		user := ""
		for _, m := range in.Messages {
			if m.Role == "user" {
				user = m.Content
			}
		}
		category := "benign"
		if strings.Contains(user, "attack") {
			category = "sqli"
		}
		content, _ := json.Marshal(map[string]string{"category": category})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "stub", "object": "chat.completion", "model": "stub",
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": string(content)},
			}},
		})
	}
}

package ai

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// The exact prompts the model was trained on, exported by the aigen training
// repo (go/aigen/prompts.json); editing them here lowers quality.
//
//go:embed prompts.json
var promptsJSON []byte

type taskPrompt struct {
	System      string   `json:"system"`
	Template    string   `json:"template"`
	ReplySuffix string   `json:"reply_suffix"`
	UserCap     int      `json:"user_cap"`
	ReplyCap    int      `json:"reply_cap"`
	MaxWords    int      `json:"max_words"`
	Generic     []string `json:"generic"`
}

var prompts struct {
	Title                  taskPrompt `json:"title"`
	Rebuild                taskPrompt `json:"rebuild"`
	Fold                   taskPrompt `json:"fold"`
	SummaryMaxLen          int        `json:"summary_max_len"`
	TranscriptUserCap      int        `json:"transcript_user_cap"`
	TranscriptAssistantCap int        `json:"transcript_assistant_cap"`
}

var genericTitles = map[string]bool{}

func init() {
	if err := json.Unmarshal(promptsJSON, &prompts); err != nil {
		panic("ai: prompts.json: " + err.Error())
	}
	for _, g := range prompts.Title.Generic {
		genericTitles[g] = true
	}
}

// Result is one generation. Text is set even when the error is ErrInvalid.
type Result struct {
	Text             string
	Valid            bool // format only: the model can still state a proposal as done
	PromptTokens     int
	CompletionTokens int
	Duration         time.Duration
}

// client calls llama-server's OpenAI-compatible chat endpoint.
type client struct {
	baseURL string
}

func (c *client) title(ctx context.Context, firstUser, reply string) (Result, error) {
	p := prompts.Title
	user := strings.ReplaceAll(p.Template, "{text}", clipHead(strings.TrimSpace(firstUser), p.UserCap))
	if reply = strings.TrimSpace(reply); reply != "" {
		user += strings.ReplaceAll(p.ReplySuffix, "{reply}", clipHead(reply, p.ReplyCap))
	}
	r, err := c.chat(ctx, p.System, user, 24)
	r.Text = cleanTitle(r.Text)
	r.Valid = err == nil && validTitle(r.Text)
	return r, err
}

func (c *client) rebuild(ctx context.Context, transcript string) (Result, error) {
	p := prompts.Rebuild
	r, err := c.chat(ctx, p.System, strings.ReplaceAll(p.Template, "{text}", strings.TrimSpace(transcript)), 120)
	r.Valid = err == nil && validSummary(r.Text)
	return r, err
}

func (c *client) fold(ctx context.Context, previous, exchange string) (Result, error) {
	p := prompts.Fold
	user := strings.NewReplacer("{previous}", strings.TrimSpace(previous), "{text}", strings.TrimSpace(exchange)).Replace(p.Template)
	r, err := c.chat(ctx, p.System, user, 120)
	r.Valid = err == nil && validSummary(r.Text)
	return r, err
}

func validTitle(s string) bool {
	return s != "" && len(strings.Fields(s)) <= prompts.Title.MaxWords && !genericTitles[strings.ToLower(s)]
}

func validSummary(s string) bool {
	s = strings.TrimSpace(s)
	return s != "" && len(s) <= prompts.SummaryMaxLen
}

func cleanTitle(raw string) string {
	return strings.Trim(strings.Join(strings.Fields(raw), " "), "\"'`.,!?;:()[]{}<> ")
}

// Runes, not bytes: a cut through a multi-byte character renders as a
// replacement glyph.
func clipHead(s string, n int) string {
	if r := []rune(s); n > 0 && len(r) > n {
		return strings.TrimSpace(string(r[:n])) + "…"
	}
	return s
}

func clipTail(s string, n int) string {
	if r := []rune(s); n > 0 && len(r) > n {
		return "…" + strings.TrimSpace(string(r[len(r)-n:]))
	}
	return s
}

type turn struct {
	role, text string
}

// renderTranscript clips the way training did: a user's ask from its head, an
// agent's reply from its tail, where the outcome is.
func renderTranscript(turns []turn) string {
	var lines []string
	for _, t := range turns {
		text := strings.TrimSpace(t.text)
		switch {
		case text == "":
		case t.role == "user":
			lines = append(lines, "User: "+clipHead(text, prompts.TranscriptUserCap))
		default:
			lines = append(lines, "Assistant: "+clipTail(text, prompts.TranscriptAssistantCap))
		}
	}
	return strings.Join(lines, "\n")
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (c *client) chat(ctx context.Context, system, user string, maxTokens int) (Result, error) {
	body, err := json.Marshal(map[string]any{
		"messages":    []chatMessage{{"system", system}, {"user", user}},
		"max_tokens":  maxTokens,
		"temperature": 0, // greedy, as evaluated
	})
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message chatMessage `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Result{}, fmt.Errorf("ai: decode: %w", err)
	}
	r := Result{Duration: time.Since(start)}
	switch {
	case out.Error != nil:
		// llama-server can reject its own output (a chat-format parse error).
		return r, fmt.Errorf("ai: llama-server: %s (HTTP %d)", out.Error.Message, resp.StatusCode)
	case len(out.Choices) == 0:
		return r, fmt.Errorf("ai: empty response (HTTP %d)", resp.StatusCode)
	}
	r.Text = strings.TrimSpace(out.Choices[0].Message.Content)
	r.PromptTokens, r.CompletionTokens = out.Usage.PromptTokens, out.Usage.CompletionTokens
	return r, nil
}

package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestPromptsAreEmbedded(t *testing.T) {
	if !strings.HasPrefix(prompts.Title.System, "Generate a short chat title") {
		t.Fatalf("title prompt not loaded: %.40q", prompts.Title.System)
	}
	if !strings.Contains(prompts.Fold.Template, "{previous}") || !strings.Contains(prompts.Rebuild.Template, "{text}") {
		t.Fatal("summary templates lost their placeholders")
	}
	if prompts.Title.MaxWords == 0 || prompts.SummaryMaxLen == 0 || prompts.TranscriptAssistantCap == 0 {
		t.Fatalf("caps not loaded: %+v", prompts)
	}
}

func TestValidTitle(t *testing.T) {
	for in, want := range map[string]bool{
		"Fix Login Bug": true,
		"New Chat":    false, // the model's "too vague" answer
		"":              false,
		"Build A Multi Tenant Support Knowledge Base": false,
	} {
		if got := validTitle(cleanTitle(in)); got != want {
			t.Errorf("validTitle(%q) = %v, want %v", in, got, want)
		}
	}
	if got := cleanTitle("  \"Fix Login\n Bug.\" "); got != "Fix Login Bug" {
		t.Errorf("cleanTitle = %q", got)
	}
}

func TestRenderTranscriptClipsAskFromHeadAndReplyFromTail(t *testing.T) {
	long := strings.Repeat("é", prompts.TranscriptAssistantCap) + " done"
	got := renderTranscript([]turn{{"user", " fix it "}, {"assistant", ""}, {"assistant", long}})
	lines := strings.Split(got, "\n")
	if len(lines) != 2 || lines[0] != "User: fix it" {
		t.Fatalf("got %q", got)
	}
	if !strings.HasPrefix(lines[1], "Assistant: …") || !strings.HasSuffix(lines[1], " done") {
		t.Fatalf("reply not clipped from its tail: %.60q", lines[1])
	}
}

// fakeLlama answers every chat with reply and records the user prompts it got.
func fakeLlama(t *testing.T, reply string) (*client, *[]string) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []chatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) != 2 {
			t.Errorf("bad request: %v %+v", err, req)
		}
		got = append(got, req.Messages[1].Content)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": reply}}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 3},
		})
	}))
	t.Cleanup(srv.Close)
	return &client{baseURL: srv.URL}, &got
}

func TestSummaryFoldsIntoThePreviousNote(t *testing.T) {
	c, got := fakeLlama(t, "You asked to fix login. It is fixed.")
	r, err := c.fold(context.Background(), "You asked to fix login. Not started.", renderTranscript([]turn{{"user", "go"}, {"assistant", "Fixed."}}))
	if err != nil || !r.Valid || r.PromptTokens != 10 {
		t.Fatalf("fold = %+v, %v", r, err)
	}
	if p := (*got)[0]; !strings.Contains(p, "Previous summary:\nYou asked to fix login. Not started.") || !strings.Contains(p, "User: go\nAssistant: Fixed.") {
		t.Fatalf("fold prompt = %q", p)
	}
}

func TestInvalidOutputIsAnError(t *testing.T) {
	c, _ := fakeLlama(t, "New Chat")
	r, err := check(c.title(context.Background(), "hi", ""))
	if !errors.Is(err, ErrInvalid) || r.Text != "New Chat" {
		t.Fatalf("title = %+v, %v; want ErrInvalid with the text kept", r, err)
	}
}

func TestServerErrorIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":{"message":"parse failed"}}`))
	}))
	defer srv.Close()
	c := &client{baseURL: srv.URL}
	if r, err := c.rebuild(context.Background(), "User: hi"); err == nil || r.Valid || !strings.Contains(err.Error(), "parse failed") {
		t.Fatalf("rebuild = %+v, %v", r, err)
	}
}

func TestUnbundledBuildSaysSo(t *testing.T) {
	if bundleVersion != "" {
		t.Skip("model bundled")
	}
	if _, err := New(t.TempDir(), false).Title(context.Background(), "fix it", ""); !errors.Is(err, ErrNotBundled) {
		t.Fatalf("err = %v, want ErrNotBundled", err)
	}
}

// The TestModel tests run the real model and log what it wrote. They need a
// tagged build: scripts/ai-bundle.sh <dir> && go test -tags aigen -v -run TestModel ./internal/ai
var shared struct {
	once  sync.Once
	model *Model
	dir   string
}

func TestMain(m *testing.M) {
	code := m.Run()
	if shared.model != nil {
		shared.model.Close()
		os.RemoveAll(shared.dir)
	}
	os.Exit(code)
}

// bundledModel is one model for every test: unpacking 400 MB takes seconds.
func bundledModel(t *testing.T) *Model {
	if bundleVersion == "" {
		t.Skip("model not bundled; build with -tags aigen (scripts/ai-bundle.sh)")
	}
	shared.once.Do(func() {
		dir, err := os.MkdirTemp("", "repogo-ai-")
		if err != nil {
			t.Fatal(err)
		}
		shared.dir, shared.model = dir, New(dir, false)
	})
	return shared.model
}

func TestModelTitles(t *testing.T) {
	m := bundledModel(t)
	for _, tc := range []struct{ user, reply string }{
		{"fix the login bug in auth.ts where a null session crashes the app", ""},
		{"why is my useEffect running twice in dev", "In React 18 StrictMode, effects run twice in development to surface missing cleanups."},
		{"refactor apps/api/src/routes/socket.ts to use zod", ""},
		{"the fly deploy fails with a health check timeout", ""},
	} {
		r, err := m.Title(context.Background(), tc.user, tc.reply)
		t.Logf("%-60.60q → %q (%s, %d+%d tokens)", tc.user, r.Text, r.Duration, r.PromptTokens, r.CompletionTokens)
		if err != nil {
			t.Errorf("Title(%q): %v", tc.user, err)
		}
	}
}

// The prompt asks for "New Chat" (invalid, so the caller falls back) on a
// greeting; the model doesn't reliably do it yet, so this reports rather than fails.
func TestModelTitleOfAGreeting(t *testing.T) {
	r, err := bundledModel(t).Title(context.Background(), "hi", "")
	t.Logf("hi → %q, %v", r.Text, err)
}

func TestModelTurnSummaries(t *testing.T) {
	m := bundledModel(t)
	ctx := context.Background()
	turns := []struct{ user, reply string }{
		{"the login page crashes when the session is null, fix it in auth.ts",
			"I found the crash: `session.user` is read before the null check in auth.ts. I added a guard and a test. The login page now loads with no session."},
		{"also redirect to /signin when there's no session",
			"Added a redirect to /signin in the auth middleware when the session is missing. Tests pass."},
		{"deploy it", "I tried `fly deploy` but it failed: the health check timed out on port 8080. The app listens on 3000."},
	}
	var summary string
	for i, tc := range turns {
		r, err := m.TurnSummary(ctx, summary, tc.user, tc.reply)
		t.Logf("turn %d → %q (%s)", i+1, r.Text, r.Duration)
		if err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
		if !strings.HasPrefix(r.Text, "You ") {
			t.Errorf("turn %d: summary %q is not in the second person", i+1, r.Text)
		}
		summary = r.Text
	}
}

func TestModelRestartsAfterClose(t *testing.T) {
	m := bundledModel(t)
	ctx := context.Background()
	if _, err := m.Title(ctx, "add dark mode to the settings screen", ""); err != nil {
		t.Fatal(err)
	}
	m.Close()
	if _, err := m.Title(ctx, "add dark mode to the settings screen", ""); err != nil {
		t.Fatalf("after Close: %v", err)
	}
}

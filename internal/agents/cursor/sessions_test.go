package cursor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/clilogin"
	"github.com/repogo/host/internal/cursoragent"
	"github.com/repogo/host/internal/session"
)

func TestNativeSessionRenameDeleteAndRebuild(t *testing.T) {
	p := testProvider(t)
	req := testRequest(t, "original")
	result, err := p.Send(t.Context(), req, agent.TurnIO{TurnID: "first", Emit: func(agent.Event) {}})
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	metas, err := p.sessions.List()
	if err != nil || len(metas) != 1 {
		t.Fatalf("%+v %v", metas, err)
	}
	if err = p.sessions.Rename(metas[0], "renamed"); err != nil {
		t.Fatal(err)
	}
	metas, err = p.sessions.List()
	if err != nil || metas[0].Title != "renamed" {
		t.Fatalf("%+v %v", metas, err)
	}
	events, err := p.sessions.Snapshot(metas[0])
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, e := range events {
		if e.Kind == agent.EventText {
			text += e.Text
		}
	}
	if text != "original" {
		t.Fatalf("native replay %q", text)
	}
	if err = p.sessions.Delete(metas[0]); err != nil {
		t.Fatal(err)
	}
	dir, _ := p.sessions.directory(result.SessionID)
	if _, err = os.Stat(filepath.Join(dir, "store.db")); !os.IsNotExist(err) {
		t.Fatalf("native state survived: %v", err)
	}
	if _, err = p.sessions.directory("../escape"); err == nil {
		t.Fatal("accepted traversal")
	}
}

func TestCatalogUsesAdvertisedIDs(t *testing.T) {
	var state cursoragent.Session
	raw := `{"models":{"currentModelId":"composer[fast=true]","availableModels":[{"modelId":"composer[fast=true]","name":"Composer"}]},"modes":{"availableModes":[{"id":"ask","name":"Ask"}]}}`
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	c := catalog(state, true)
	if !c.Available || c.Models[0].ID != "composer[fast=true]" || !c.Models[0].Default || c.Modes[0].Value != "ask" || len(c.Models[0].Modalities) != 2 {
		t.Fatalf("%+v", c)
	}
}
func TestInvalidApprovalAndQuestionAnswersNeverAllow(t *testing.T) {
	s := &liveSession{id: fakeID, turnCtx: t.Context(), denied: map[string]bool{}, tools: map[string]cursoragent.ToolCall{}, io: &agent.TurnIO{Ask: func(context.Context, agent.Approval) (json.RawMessage, error) {
		return json.RawMessage(`"invented"`), nil
	}}}
	r, err := s.permissionRequest(t.Context(), cursoragent.Permission{SessionID: fakeID, ToolCall: cursoragent.ToolCall{ToolCallID: "t"}, Options: []cursoragent.PermissionOption{{OptionID: "allow-once", Kind: "allow_once"}}})
	if err == nil || r.Outcome.Outcome != "cancelled" || !s.denied["t"] {
		t.Fatalf("%+v %v", r, err)
	}
	s.io.Ask = func(context.Context, agent.Approval) (json.RawMessage, error) {
		return json.RawMessage(`{"color":"Green"}`), nil
	}
	q, err := s.questionRequest(t.Context(), cursoragent.Questions{Questions: []cursoragent.Question{{ID: "color", Options: []cursoragent.QuestionOption{{ID: "blue", Label: "Blue"}}}}})
	if err == nil || q.Outcome.Outcome != "cancelled" {
		t.Fatalf("%+v %v", q, err)
	}
}
func TestLoginLinkAndEnvironmentIsolation(t *testing.T) {
	p := testProvider(t)
	var code clilogin.Code
	if loginLine("https://unrelated.example/login", &code) {
		t.Fatal("accepted unrelated login")
	}
	if !loginLine("Open https://cursor.com/loginDeepControl?uuid=test", &code) {
		t.Fatalf("%+v", code)
	}
	for _, kv := range p.environment() {
		if strings.HasPrefix(kv, "CURSOR_API_KEY=") || strings.HasPrefix(kv, "CURSOR_AUTH_TOKEN=") {
			t.Fatal("inherited live auth in isolated provider")
		}
	}
	spec := p.Definition().Tool.Spec
	if spec.Command != "cursor-agent" || spec.AuthProbe == nil || strings.Join(spec.NativeUpdate, " ") != "cursor-agent update" {
		t.Fatalf("bad install: %s", spec.Command)
	}
	var _ session.Provider = p.sessions
}

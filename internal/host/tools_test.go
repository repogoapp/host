package host_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/host"
	"github.com/repogoapp/host/internal/repogomcp"
	"github.com/repogoapp/host/internal/testhost"
)

type idleSession struct{}

func (idleSession) Running() (agent.RunningTurn, bool) { return agent.RunningTurn{}, false }

// RepoGo's tools reach the MCP servers opened after them: an agent's
// initialize and tools/list are answered, not dropped.
func TestToolsAnswerThroughTheHostsWiring(t *testing.T) {
	h, err := host.New(t.Context(), testhost.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	entry, release := h.Tools().Attach(idleSession{}, agent.TurnRequest{Cwd: t.TempDir()})
	defer release()
	for _, method := range []string{"initialize", "tools/list"} {
		req := httptest.NewRequest(http.MethodPost, repogomcp.Path,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":{}}`))
		req.Header.Set("Authorization", entry.Headers[0].Value)
		w := httptest.NewRecorder()
		h.Tools().ServeHTTP(w, req)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"result"`) {
			t.Fatalf("%s: %d %s", method, w.Code, w.Body)
		}
	}
}

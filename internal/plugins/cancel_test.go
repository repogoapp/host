package plugins

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/testwait"
	"github.com/repogoapp/host/tunnelwire"
)

// waits marks that it started, waits for its signal, and only a moment later
// marks that it saw the abort and answers: the caller must wait for that.
const waitsPlugin = `import { definePlugin, method, z } from "@repogo/plugins";
export default definePlugin({
  methods: {
    waits: method(z.object({ started: z.string(), aborted: z.string() }), async (input, ctx) => {
      await Bun.write(input.started, "1");
      await new Promise((resolve) => ctx.signal.addEventListener("abort", resolve, { once: true }));
      await Bun.sleep(200);
      await Bun.write(input.aborted, "1");
      return "stopped";
    }, { http: { public: "none" } }),
  },
});
`

// A public caller that leaves aborts the call's ctx.signal, and ServeHTTP
// returns only once the plugin has answered, so its caller's place stays taken.
func TestLeavingCallerAbortsAndWaitsForThePlugin(t *testing.T) {
	dir := sdk(t)
	s, _ := newService(t, dir)
	s.cfg.Global = filepath.Join(project(t, dir, map[string]string{"slow": waitsPlugin}), ".repogo", "plugins")
	marks := t.TempDir()
	started, aborted := filepath.Join(marks, "started"), filepath.Join(marks, "aborted")
	body := `{"started":` + strconv.Quote(started) + `,"aborted":` + strconv.Quote(aborted) + `}`
	route, _ := tunnelwire.ParsePluginPath("/slow/waits")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.ServeHTTP(ctx, route, httptest.NewRequest(http.MethodPost, "https://h.example/slow/waits", strings.NewReader(body)))
		done <- err
	}()
	testwait.For(t, "the call to start", func() bool { _, err := os.Stat(started); return err == nil })
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(testwait.Timeout):
		t.Fatal("ServeHTTP never returned after its caller left")
	}
	if _, err := os.Stat(aborted); err != nil {
		t.Fatal("ServeHTTP returned before the plugin answered its abort")
	}
}

package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/rpc"
)

// A method's every dot is a slash in the path: two-part and three-part
// names both reach the router.
func TestCallReachesMethodsOfAnyDepth(t *testing.T) {
	r := rpc.New(slog.Default())
	ok := func(context.Context, rpc.Caller, rpc.None) (rpc.Ack, error) { return rpc.OK, nil }
	rpc.Add(r, "t.two", ok)
	rpc.Add(r, "t.sub.three", ok)
	mux := http.NewServeMux()
	New(r, "").Register(mux)
	for _, path := range []string{"/v1/rpc/t/two", "/v1/rpc/t/sub/three"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
}

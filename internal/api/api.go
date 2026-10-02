// Package api is the loopback HTTP transport onto internal/rpc.
package api

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/jsonrpc"
	"github.com/repogo/host/internal/rpc"
)

type API struct {
	router *rpc.Router
	self   device.ID
}

func New(router *rpc.Router, self device.ID) *API {
	return &API{router: router, self: self}
}

func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/rpc/{family}/{method}", a.call)
}

// call is the entire JSON surface. Scope is local: only the loopback listener
// behind the token guard reaches it.
func (a *API) call(w http.ResponseWriter, r *http.Request) {
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, jsonrpc.MaxMessageBytes))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, err)
		return
	}

	out, err := a.router.Call(r.Context(),
		rpc.Caller{Device: a.self, Scope: rpc.ScopeLocal},
		r.PathValue("family")+"."+r.PathValue("method"), payload)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func statusFor(err error) int {
	switch rpc.Code(err) {
	case jsonrpc.CodeMethodNotFound:
		return http.StatusNotImplemented
	case jsonrpc.CodeUnavailable:
		return http.StatusServiceUnavailable
	case jsonrpc.CodeInvalidParams:
		return http.StatusBadRequest
	case jsonrpc.CodeDenied:
		return http.StatusForbidden
	case jsonrpc.CodeNotFound:
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
}

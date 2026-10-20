package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/repogoapp/host/internal/tunnel"
	"github.com/repogoapp/host/tunnelwire"
)

// The environment's own address serves the environment's plugins, never a
// project's: it names no project. A method answers there only when it opts in
// with `http`, isn't a stream and asks no confirm, since no one is there to.

// CallerHTTP is a call that came in at the environment's address.
const CallerHTTP Caller = "http"

// The access a method opts into: the host checks a key, or the method checks
// the caller itself.
const (
	httpKey  = "key"
	httpNone = "none"
)

// routesWait bounds reading the manifest to decide whether to stay on the gateway.
const routesWait = 10 * time.Second

// HasRoutes reports whether any environment plugin serves HTTP.
func (s *Service) HasRoutes() bool {
	ctx, cancel := context.WithTimeout(s.ctx, routesWait)
	defer cancel()
	found, err := s.globalPlugins(ctx)
	if err != nil {
		return false
	}
	for _, p := range found {
		for _, m := range p.Methods {
			if open(m.HTTP, m.Kind, m.Confirm) {
				return true
			}
		}
	}
	return false
}

// ServeHTTP runs the method route names with the request's JSON body, or its
// query for a GET, as input, and answers its result as JSON.
func (s *Service) ServeHTTP(ctx context.Context, route tunnelwire.PluginRoute, req *http.Request) (*http.Response, error) {
	found, err := s.globalPlugins(ctx)
	if err != nil {
		return nil, err
	}
	name := route.Plugin + "." + route.Method
	var access string
	for _, p := range found {
		for _, m := range p.Methods {
			if m.Name == name && open(m.HTTP, m.Kind, m.Confirm) {
				access = m.HTTP
			}
		}
	}
	switch access {
	case "":
		return nil, tunnel.ErrNoRoute
	case httpKey:
		// Keys aren't issued yet, so a method that wants one can't be reached.
		return jsonResponse(http.StatusUnauthorized, map[string]string{"error": "This method needs a key."}), nil
	}
	input, err := httpInput(req)
	if err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()}), nil
	}
	t := Target{Dir: s.cfg.Global}
	answer, err := s.ask(ctx, t, "call", callParams{Target: t, Name: name, Input: input, Caller: CallerHTTP})
	if err != nil {
		return nil, err
	}
	if answer.Error != nil {
		return jsonResponse(httpStatus(answer.Error.Code), map[string]any{"error": answer.Error.Message, "fields": answer.Error.FieldErrors}), nil
	}
	return jsonResponse(http.StatusOK, answer.Result), nil
}

// globalPlugins is the environment's plugins as their Worker lists them.
func (s *Service) globalPlugins(ctx context.Context) ([]manifestPlugin, error) {
	if !s.On() || !isDir(s.cfg.Global) {
		return nil, ErrNoPlugins
	}
	t := Target{Dir: s.cfg.Global}
	answer, err := s.ask(ctx, t, "list", t)
	if err != nil {
		return nil, err
	}
	if answer.Error != nil {
		return nil, errors.New(answer.Error.Message)
	}
	var manifest struct {
		Plugins []manifestPlugin `json:"plugins"`
	}
	if err := json.Unmarshal(answer.Result, &manifest); err != nil {
		return nil, err
	}
	return manifest.Plugins, nil
}

func open(access, kind, confirm string) bool {
	return (access == httpKey || access == httpNone) && kind != "stream" && confirm == ""
}

// httpInput is a GET's query as an object of strings, else the JSON body.
func httpInput(req *http.Request) (json.RawMessage, error) {
	if req.Method == http.MethodGet || req.Method == http.MethodHead {
		query := map[string]string{}
		for k, vs := range req.URL.Query() {
			query[k] = vs[0]
		}
		return json.Marshal(query)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return json.RawMessage("{}"), nil
	}
	if !json.Valid(body) {
		return nil, errors.New("The body must be JSON.")
	}
	return body, nil
}

func httpStatus(code string) int {
	switch code {
	case "VALIDATION":
		return http.StatusBadRequest
	case "NOT_FOUND":
		return http.StatusNotFound
	case "FORBIDDEN":
		return http.StatusForbidden
	}
	return http.StatusInternalServerError
}

func jsonResponse(status int, v any) *http.Response {
	body, err := json.Marshal(v)
	if raw, ok := v.(json.RawMessage); ok {
		body, err = raw, nil
	}
	if err != nil || len(body) == 0 {
		body = []byte("null")
	}
	return &http.Response{
		StatusCode:    status,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(strings.NewReader(string(body))),
		ContentLength: int64(len(body)),
	}
}

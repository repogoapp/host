package plugins

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/repogoapp/host/internal/tunnel"
	"github.com/repogoapp/host/tunnelwire"
)

// The environment's addresses serve the environment's plugins, never a
// project's: they name no project. A method answers at one only when it opts in
// with `http` for that address, isn't a stream and asks no confirm, since no
// one is there to.

// Address is where an HTTP request came in.
type Address string

const (
	// Public is the environment's own name, through the tunnel.
	Public Address = "public"
	// Device is this machine's loopback and its containers' gateways.
	Device Address = "device"
)

// CallerHTTP is a call that came in at the public address, CallerDevice one at the device address.
const (
	CallerHTTP   Caller = "http"
	CallerDevice Caller = "device"
)

var callers = map[Address]Caller{Public: CallerHTTP, Device: CallerDevice}

// The access a method opts into: the host checks the plugin's key, or anyone
// who reaches the address may call.
const (
	httpKey  = "key"
	httpNone = "none"
)

// routesWait bounds reading the manifest to decide whether to stay on the gateway.
const routesWait = 10 * time.Second

// maxDeviceBody bounds a device request's body: a container may be anyone's.
const maxDeviceBody = 1 << 20

// HasRoutes reports whether any environment plugin serves the public address.
func (s *Service) HasRoutes() bool {
	ctx, cancel := context.WithTimeout(s.ctx, routesWait)
	defer cancel()
	_, found, err := s.served(ctx)
	if err != nil {
		return false
	}
	for _, p := range found {
		for _, m := range p.Methods {
			if open(m.HTTP[Public], m.Stream, m.Approve) {
				return true
			}
		}
	}
	return false
}

// ServeHTTP answers the public address for route.
func (s *Service) ServeHTTP(ctx context.Context, route tunnelwire.PluginRoute, req *http.Request) (*http.Response, error) {
	return s.serve(ctx, Public, route, req)
}

// ServeDevice answers the device address: /<plugin>/<method> runs a method,
// and /<plugin> lists the methods it serves there, for a plugin elsewhere to
// stand in for it.
func (s *Service) ServeDevice(w http.ResponseWriter, req *http.Request) {
	// A web page in a browser here can reach loopback; only programs may call.
	// One whose own name was rebound to this address sends neither header, so
	// the name it called is checked too.
	if req.Header.Get("Origin") != "" || req.Header.Get("Sec-Fetch-Site") != "" || !deviceHost(req.Host) {
		writeResponse(w, jsonResponse(http.StatusForbidden, map[string]string{"error": "A browser can't call this address."}))
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, maxDeviceBody)
	var (
		resp *http.Response
		err  error
	)
	if route, ok := tunnelwire.ParsePluginPath(req.URL.Path); ok {
		resp, err = s.serve(req.Context(), Device, route, req)
	} else {
		resp, err = s.listing(req.Context(), strings.TrimPrefix(req.URL.Path, "/"))
	}
	switch {
	case errors.Is(err, tunnel.ErrNoRoute):
		resp = jsonResponse(http.StatusNotFound, map[string]string{"error": "Nothing is served at this address."})
	case err != nil:
		s.cfg.Log.Warn("plugins: device route failed", "path", req.URL.Path, "err", err)
		resp = jsonResponse(http.StatusBadGateway, map[string]string{"error": "The plugin couldn't answer this request."})
	}
	writeResponse(w, resp)
}

// deviceHost is whether host is a name for this machine a program would call:
// an address, localhost, or Docker's name for the machine. Any other name is a
// web page's, pointed here by its own DNS.
func deviceHost(host string) bool {
	name, _, err := net.SplitHostPort(host)
	if err != nil {
		name = host
	}
	name = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(name, "["), "]"))
	if _, err := netip.ParseAddr(name); err == nil {
		return true
	}
	return name == "localhost" || name == "host.docker.internal"
}

// serve runs the method route names with the request's JSON body, or its
// query for a GET, as input, and answers its result as JSON.
func (s *Service) serve(ctx context.Context, at Address, route tunnelwire.PluginRoute, req *http.Request) (*http.Response, error) {
	t, found, err := s.served(ctx)
	if err != nil {
		return nil, err
	}
	name := route.Plugin + "." + route.Method
	var access, effect string
	for _, p := range found {
		for _, m := range p.Methods {
			if m.Name == name && open(m.HTTP[at], m.Stream, m.Approve) {
				access, effect = m.HTTP[at], m.Effect
			}
		}
	}
	switch access {
	case "":
		return nil, tunnel.ErrNoRoute
	case httpKey:
		// A missing or wrong key is the same 404, so a caller can't learn the method exists.
		if ok, err := s.keyAccepted(route.Plugin, req); err != nil || !ok {
			return nil, cmp.Or(err, tunnel.ErrNoRoute)
		}
	}
	// Link previews and crawlers fetch a shared URL, so a GET may only read.
	if effect != "read" && (req.Method == http.MethodGet || req.Method == http.MethodHead) {
		resp := jsonResponse(http.StatusMethodNotAllowed, map[string]string{"error": "This method changes data; send it as a POST."})
		resp.Header.Set("Allow", http.MethodPost)
		return resp, nil
	}
	input, err := httpInput(req)
	if err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()}), nil
	}
	answer, err := s.askHeld(ctx, t, callParams{Target: t, Name: name, Input: input, Caller: callers[at]})
	if err != nil {
		return nil, err
	}
	if answer.Error != nil {
		if status := httpStatus(answer.Error.Code); status != http.StatusInternalServerError {
			return jsonResponse(status, map[string]any{"error": answer.Error.Message, "fields": answer.Error.FieldErrors}), nil
		}
		// Only what the plugin wrote for a caller leaves: an unexpected error's
		// text may hold a URL with a token, a path or another service's answer.
		s.cfg.Log.Warn("plugins: method failed over HTTP", "method", name, "caller", callers[at], "err", answer.Error.Message)
		return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "Internal server error"}), nil
	}
	return jsonResponse(http.StatusOK, answer.Result), nil
}

// listedMethod is a method as the device address lists it, named within its plugin.
type listedMethod struct {
	Name        string          `json:"name"`
	Effect      string          `json:"effect"`
	Description string          `json:"description"`
	Input       json.RawMessage `json:"input"`
	Output      json.RawMessage `json:"output"`
}

// listing is the methods plugin serves at the device address with no key, or
// ErrNoRoute when there are none: a method that needs a key isn't listed to a
// caller without one.
func (s *Service) listing(ctx context.Context, plugin string) (*http.Response, error) {
	_, found, err := s.served(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range found {
		if p.Name != plugin {
			continue
		}
		var methods []listedMethod
		for _, m := range p.Methods {
			if m.HTTP[Device] == httpNone && open(m.HTTP[Device], m.Stream, m.Approve) {
				methods = append(methods, listedMethod{Name: within(plugin, m.Name), Effect: m.Effect, Description: m.Description, Input: m.Input, Output: m.Output})
			}
		}
		if len(methods) > 0 {
			return jsonResponse(http.StatusOK, map[string]any{"description": p.Description, "methods": methods}), nil
		}
	}
	return nil, tunnel.ErrNoRoute
}

// served is the environment's plugins, whose methods may serve HTTP.
func (s *Service) served(ctx context.Context) (Target, []listedPlugin, error) {
	t, err := s.folder()
	if err != nil {
		return Target{}, nil, err
	}
	found, failure, err := s.manifest(ctx, t)
	if err == nil && failure != nil {
		err = errors.New(failure.Message)
	}
	return t, found, err
}

// within is a method's name inside its plugin: today for boppr.today.
func within(plugin, name string) string { return strings.TrimPrefix(name, plugin+".") }

func open(access string, stream bool, approve string) bool {
	return (access == httpKey || access == httpNone) && !stream && approve == ""
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

func writeResponse(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		w.Header()[k] = vs
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
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

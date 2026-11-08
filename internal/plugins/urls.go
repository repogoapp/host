package plugins

import (
	"context"
	"strconv"

	"github.com/repogoapp/host/internal/tunnel"
	"github.com/repogoapp/host/tunnelwire"
)

// ServedMethod is an environment plugin's method that answers HTTP, with the
// URL of each address it opted into.
type ServedMethod struct {
	Plugin string `json:"plugin"`
	// Method is named within the plugin.
	Method string      `json:"method"`
	Effect string      `json:"effect"`
	URLs   []MethodURL `json:"urls"`
}

// MethodURL is one address a method answers at.
type MethodURL struct {
	Address Address `json:"address"`
	URL     string  `json:"url"`
	// Key: callers send the plugin's key as Authorization: Bearer.
	Key bool `json:"key"`
}

// Served is every environment plugin method that answers HTTP, or only
// plugin's when it's set, with its URLs.
func (s *Service) Served(ctx context.Context, plugin string) ([]ServedMethod, error) {
	_, found, err := s.served(ctx)
	if err != nil {
		return nil, err
	}
	bases := map[Address]string{Public: "https://" + tunnelwire.EnvLabel(s.cfg.HostID()) + "." + tunnel.Domain}
	if s.cfg.DevicePort != 0 {
		bases[Device] = "http://127.0.0.1:" + strconv.Itoa(s.cfg.DevicePort)
	}
	out := []ServedMethod{}
	for _, p := range found {
		if plugin != "" && p.Name != plugin {
			continue
		}
		for _, m := range p.Methods {
			method := ServedMethod{Plugin: p.Name, Method: within(p.Name, m.Name), Effect: m.Effect, URLs: []MethodURL{}}
			for _, at := range []Address{Device, Public} {
				if base := bases[at]; base != "" && open(m.HTTP[at], m.Stream, m.Confirm) {
					method.URLs = append(method.URLs, MethodURL{Address: at, URL: base + "/" + p.Name + "/" + method.Method, Key: m.HTTP[at] == httpKey})
				}
			}
			if len(method.URLs) > 0 {
				out = append(out, method)
			}
		}
	}
	return out, nil
}

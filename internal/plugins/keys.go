package plugins

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/repogoapp/host/internal/errkind"
)

// ErrNoKeyedPlugin is a plugin the environment doesn't have, or one with no
// method that takes a key.
var ErrNoKeyedPlugin = errkind.New(errkind.NotFound, "plugins: no such plugin with a method that takes a key")

// KeyStore keeps each environment plugin's key: state.db's plugin_keys.
type KeyStore interface {
	PluginKey(plugin string) (key string, createdAt int64, err error)
	SetPluginKey(plugin, key string, createdAt int64) error
	DeletePluginKey(plugin string) error
}

// KeyedPlugin is an environment plugin with methods that take a key over
// HTTP, and its key; Key is empty until one is made.
type KeyedPlugin struct {
	Plugin string `json:"plugin"`
	// Methods take the key, named within the plugin.
	Methods []string `json:"methods"`
	Key     string   `json:"key"`
	// Milliseconds; zero without a key.
	CreatedAt int64 `json:"created_at"`
}

// Keys is every environment plugin with a method that takes a key, with its key.
func (s *Service) Keys(ctx context.Context) ([]KeyedPlugin, error) {
	served, err := s.Served(ctx, "")
	if err != nil {
		return nil, err
	}
	out := []KeyedPlugin{}
	for _, m := range served {
		if !slices.ContainsFunc(m.URLs, func(u MethodURL) bool { return u.Key }) {
			continue
		}
		// Served lists a plugin's methods together.
		if len(out) == 0 || out[len(out)-1].Plugin != m.Plugin {
			key, createdAt, err := s.cfg.Keys.PluginKey(m.Plugin)
			if err != nil {
				return nil, err
			}
			out = append(out, KeyedPlugin{Plugin: m.Plugin, Key: key, CreatedAt: createdAt})
		}
		out[len(out)-1].Methods = append(out[len(out)-1].Methods, m.Method)
	}
	return out, nil
}

// CreateKey makes plugin a new key, replacing the one it had: callers holding
// the old one are refused from now on.
func (s *Service) CreateKey(ctx context.Context, plugin string) (KeyedPlugin, error) {
	keyed, err := s.Keys(ctx)
	if err != nil {
		return KeyedPlugin{}, err
	}
	i := slices.IndexFunc(keyed, func(k KeyedPlugin) bool { return k.Plugin == plugin })
	if i < 0 {
		return KeyedPlugin{}, ErrNoKeyedPlugin
	}
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	k := keyed[i]
	k.Key, k.CreatedAt = "rgk_"+base64.RawURLEncoding.EncodeToString(secret), time.Now().UnixMilli()
	if err := s.cfg.Keys.SetPluginKey(plugin, k.Key, k.CreatedAt); err != nil {
		return KeyedPlugin{}, err
	}
	return k, nil
}

// DeleteKey removes plugin's key, so its methods that take one answer no one.
func (s *Service) DeleteKey(plugin string) error {
	return s.cfg.Keys.DeletePluginKey(plugin)
}

// keyAccepted reports whether req carries plugin's key as a bearer token.
func (s *Service) keyAccepted(plugin string, req *http.Request) (bool, error) {
	sent, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if !ok || sent == "" {
		return false, nil
	}
	key, _, err := s.cfg.Keys.PluginKey(plugin)
	if err != nil || key == "" {
		return false, err
	}
	return subtle.ConstantTimeCompare([]byte(sent), []byte(key)) == 1, nil
}

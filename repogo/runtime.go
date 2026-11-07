package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/repogoapp/host/internal/apphome"
)

// localConf is written 0600; reading it is the entire local authentication story.
type localConf struct {
	Port     int    `json:"port"`
	Token    string `json:"token"`
	ServerID string `json:"server_id"`
	// LAN serves phones on this machine's local network address too, set by
	// `repogo start --lan`. Off unless asked: anyone on the network can reach the port.
	LAN bool `json:"lan,omitempty"`
	// NoTailscale keeps the host off its tailnet, set by `repogo start
	// --tailscale=false`: on a shared or work tailnet every member reaches the port.
	NoTailscale bool `json:"no_tailscale,omitempty"`
}

func confPath() (string, error) {
	path, err := apphome.Path("runtime.json")
	if err != nil {
		return "", fmt.Errorf("locate state dir: %w", err)
	}
	return path, nil
}

func loadOrCreateConf(path string) (*localConf, error) {
	var c localConf
	found, err := apphome.ReadJSON(path, &c)
	switch {
	case !found && err != nil:
		return nil, err
	case err == nil && c.Token != "" && c.ServerID != "":
		return &c, nil
	}
	// A corrupt file is regenerated: clients re-read it, refusing to start is worse.

	c = localConf{Token: randomHex(32), ServerID: randomHex(8)}
	if err := writeConf(path, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// saveDirect records which direct paths the host serves, --lan and
// --tailscale as given (nil leaves one as it is), and reports whether
// anything changed, so a running host restarts only when it did.
func saveDirect(lan, tailscale *bool) (bool, error) {
	path, err := confPath()
	if err != nil {
		return false, err
	}
	c, err := loadOrCreateConf(path)
	if err != nil {
		return false, err
	}
	was := *c
	if lan != nil {
		c.LAN = *lan
	}
	if tailscale != nil {
		c.NoTailscale = !*tailscale
	}
	if *c == was {
		return false, nil
	}
	return true, writeConf(path, c)
}

func writeConf(path string, c *localConf) error {
	return apphome.WriteJSON(path, c, 0o600)
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

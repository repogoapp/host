package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/repogo/host/internal/apphome"
)

// localConf is written 0600; reading it is the entire local authentication story.
type localConf struct {
	Port     int    `json:"port"`
	Token    string `json:"token"`
	ServerID string `json:"server_id"`
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

func writeConf(path string, c *localConf) error {
	return apphome.WriteJSON(path, c, 0o600)
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

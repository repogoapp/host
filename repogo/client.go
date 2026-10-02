package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/hostinfo"
	"github.com/repogo/host/internal/release"
)

func call(ctx context.Context, method string, params, out any) error {
	return callWithin(ctx, 8*time.Second, method, params, out)
}

func callWithin(ctx context.Context, timeout time.Duration, method string, params, out any) error {
	path, err := confPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("host is not running; run repogo start")
	}
	var conf localConf
	if err := json.Unmarshal(data, &conf); err != nil {
		return err
	}
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/v1/rpc/%s", conf.Port, strings.ReplaceAll(method, ".", "/")), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+conf.Token)
	req.Header.Set("Content-Type", "application/json")
	// Local authentication must never follow an HTTP redirect.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("host is not reachable; run repogo start or repogo logs")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var failure struct {
			Error string `json:"error"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&failure); err != nil {
			return fmt.Errorf("host: %s", resp.Status)
		}
		return fmt.Errorf("host: %s", failure.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

func waitReady(ctx context.Context, version string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		var status hostinfo.Status
		if err := call(ctx, "host.status", nil, &status); err == nil && (version == "" || status.Version == version) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("host did not become ready: %w", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func peers(ctx context.Context) ([]device.Peer, error) {
	var out struct {
		Peers []device.Peer `json:"peers"`
	}
	err := call(ctx, "devices.list", nil, &out)
	return out.Peers, err
}

func printStatus(ctx context.Context) error {
	var status hostinfo.Status
	if err := call(ctx, "host.status", nil, &status); err != nil {
		return err
	}
	paired, err := peers(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("repogo %s · up %s · relay connected: %t\n", status.Version, (time.Duration(status.Uptime) * time.Second).String(), status.RelayConnected)
	for _, p := range paired {
		fmt.Printf("  %s (%s)\n", p.Label, p.ID)
	}
	return nil
}

// noteOlderHost says when the background host still runs an older binary than this
// one, which it keeps until restarted; restarting stops its running chats.
func noteOlderHost(ctx context.Context) {
	var status hostinfo.Status
	if err := call(ctx, "host.status", nil, &status); release.Version != "dev" && err == nil && status.Version != release.Version {
		fmt.Printf("The running host is %s and this is %s. Restart the host to switch (it stops running chats).\n", status.Version, release.Version)
	}
}

func pairOrStatus(ctx context.Context, force bool, pairHost string) error {
	noteOlderHost(ctx)
	paired, err := peers(ctx)
	if err != nil {
		return err
	}
	if len(paired) > 0 && !force {
		return printStatus(ctx)
	}
	// Keyed by AddedAt too: re-pairing a known phone keeps its ID but restamps it.
	before := make(map[device.ID]int64)
	for _, p := range paired {
		before[p.ID] = p.AddedAt
	}
	var result struct {
		QR string `json:"qr"`
	}
	if err := call(ctx, "pair.begin", nil, &result); err != nil {
		return err
	}
	if err := printPairingQR(ctx, result.QR, pairHost); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
		var status struct {
			Pending bool `json:"pending"`
		}
		if err := call(ctx, "pair.status", nil, &status); err != nil {
			return err
		}
		paired, err := peers(ctx)
		if err != nil {
			return err
		}
		for _, p := range paired {
			if added, ok := before[p.ID]; !ok || added != p.AddedAt {
				fmt.Printf("Paired with %s; the host keeps running in the background.\n", p.Label)
				return nil
			}
		}
		if !status.Pending {
			return fmt.Errorf("pairing expired; run repogo pair for a new code")
		}
	}
}

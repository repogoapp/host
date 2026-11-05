// Command hostpassthrough checks a running host's passthrough path live: it
// pairs a throwaway device over the host's loopback socket (as a phone would
// after scanning), then reaches the host through the passthrough relay over
// the pinned, session-bound TLS the invite named, and calls it there.
//
//	REPOGO_HOME=<the test host's home> go run ./bench/hostpassthrough [-relay h.repogo.app:443]
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/repogoapp/host/internal/apphome"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/hostclient"
	"github.com/repogoapp/host/internal/rpc/pairing"
)

func main() {
	relay := flag.String("relay", "h.repogo.app:443", "the passthrough relay")
	flag.Parse()
	if err := run(*relay); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

func run(relay string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var conf struct {
		Port  int    `json:"port"`
		Token string `json:"token"`
	}
	home, err := apphome.Dir()
	if err != nil {
		return err
	}
	if _, err := apphome.ReadJSON(filepath.Join(home, "runtime.json"), &conf); err != nil || conf.Port == 0 {
		return fmt.Errorf("no running host in %s: %v", home, err)
	}
	phone, err := device.Generate()
	if err != nil {
		return err
	}

	// 1. Pair over loopback with the runtime token, as the QR flow ends.
	local, err := hostclient.Dial(ctx, hostclient.Config{
		URL: fmt.Sprintf("ws://127.0.0.1:%d/ws", conf.Port), Identity: phone,
		Unsigned: true, LocalToken: conf.Token, Label: "passthrough check", Platform: "ios",
	})
	if err != nil {
		return fmt.Errorf("loopback: %w", err)
	}
	var begun pairing.BeginResult
	if err := local.Call(ctx, "pair.begin", nil, &begun); err != nil {
		return fmt.Errorf("pair.begin: %w", err)
	}
	invite, err := device.DecodeInvite(begun.QR)
	if err != nil {
		return err
	}
	if invite.TLSPin == nil {
		return errors.New("the invite has no tls_pin: is REPOGO_HRELAY set on the host?")
	}
	if err := device.VerifyPin(invite.InviterPublic, *invite.TLSPin); err != nil {
		return fmt.Errorf("invite pin: %w", err)
	}
	fmt.Printf("invite: host %s, pin %x… (counter %d), signed by the host ✓\n", invite.InviterID, invite.TLSPin.SPKI[:6], invite.TLSPin.Counter)
	var done pairing.CompleteResult
	if err := local.Call(ctx, "pair.complete", pairing.CompleteParams{
		DeviceID: string(phone.ID), PublicKey: phone.Public, Label: "passthrough check", Platform: "ios",
		Proof: device.Proof(invite.Code, phone.Public),
	}, &done); err != nil {
		return fmt.Errorf("pair.complete: %w", err)
	}
	if !hmac.Equal(done.HostProof, device.Proof(invite.Code, invite.InviterPublic)) {
		return errors.New("the host's pairing proof does not match the invite")
	}
	local.Close()
	fmt.Printf("paired: device %s\n", phone.ID)

	// 2. Reach the host by name through the relay, pinned and bound.
	name, _, _ := net.SplitHostPort(relay)
	target := string(invite.InviterID) + "." + name
	pin := invite.TLSPin.SPKI
	var group string
	start := time.Now()
	remote, err := hostclient.Dial(ctx, hostclient.Config{
		URL: "wss://" + target + "/ws", Identity: phone, GroupID: done.GroupID, Label: "passthrough check", Platform: "ios",
		TLS: &tls.Config{
			ServerName: target, MinVersion: tls.VersionTLS13,
			// The pin is the only trust: CA checks are off and the key must match.
			InsecureSkipVerify: true,
			VerifyConnection: func(cs tls.ConnectionState) error {
				group = cs.CurveID.String()
				if len(cs.PeerCertificates) == 0 || !hmac.Equal(sum(cs.PeerCertificates[0].RawSubjectPublicKeyInfo), pin) {
					return errors.New("certificate does not match the pin")
				}
				return nil
			},
		},
	})
	if err != nil {
		return fmt.Errorf("passthrough: %w", err)
	}
	defer remote.Close()
	fmt.Printf("connected: %s in %v, TLS 1.3 %s, hello bound to the session ✓ (host %s)\n",
		target, time.Since(start).Round(time.Millisecond), group, remote.ServerVersion())

	var status json.RawMessage
	if err := remote.Call(ctx, "host.status", nil, &status); err != nil {
		return fmt.Errorf("host.status: %w", err)
	}
	fmt.Printf("calls over passthrough: host.status %d bytes ✓\n", len(status))

	return nil
}

func sum(b []byte) []byte { s := sha256.Sum256(b); return s[:] }

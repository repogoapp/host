package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	"github.com/repogo/host/internal/device"
)

func printPairingQR(ctx context.Context, payload, pairHost string, reusable bool) error {
	invite, err := device.DecodeInvite(payload)
	if err != nil {
		return err
	}

	pairingCode, err := registerInvite(pairHost, payload, invite, reusable)
	if err != nil {
		return fmt.Errorf("register pairing code at %s: %w", pairHost, err)
	}
	link := strings.TrimRight(pairHost, "/") + "/" + pairingCode

	code, err := qrcode.New(link, qrcode.Medium)
	if err != nil {
		return err
	}
	fmt.Print("\n" + terminalQR(code))
	if inlineImages() {
		if svg := appClipCode(ctx, link); svg != nil {
			fmt.Print("\n" + inlineImage(svg, "clip.svg") + "\n")
		}
	}
	expires := time.UnixMilli(invite.ExpiresAt)
	if reusable {
		fmt.Printf("  Scan with Camera    any number of devices until %s\n", expires.Format("Jan 2 15:04 MST"))
	} else {
		fmt.Printf("  Scan with Camera    expires in %v\n", time.Until(expires).Round(time.Second))
	}
	fmt.Printf("  Code    %s\n", pairingCode)
	fmt.Printf("  Link    %s\n\n", link)
	return nil
}

// printInvite is pairing for a host nobody can look at, such as a cloud
// sandbox: it waits for the running host and writes one invite as a JSON line
// for the program that provisioned it, with no QR and nothing sent to a pair host.
func printInvite(ctx context.Context, w io.Writer) error {
	if err := waitReady(ctx, ""); err != nil {
		return err
	}
	var result struct {
		QR     string        `json:"qr"`
		Invite device.Invite `json:"invite"`
	}
	if err := call(ctx, "pair.begin", nil, &result); err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(map[string]any{
		"invite":     result.QR,
		"host_id":    result.Invite.InviterID,
		"expires_at": result.Invite.ExpiresAt,
	})
}

// registerInvite parks the invite on the pair host for its own lifetime and
// returns the code. The host gets only the code's hash and the invite sealed
// under the code; the code itself lives in the QR.
func registerInvite(pairHost, payload string, invite device.Invite, reusable bool) (string, error) {
	code := device.NewCode()
	sealed, err := device.SealInvite(code, payload)
	if err != nil {
		return "", err
	}
	body := map[string]any{"lookup": device.CodeLookup(code), "sealed": sealed}
	if reusable {
		body["reusable"] = true
		body["ttlMs"] = time.Until(time.UnixMilli(invite.ExpiresAt)).Milliseconds()
	}
	encoded, _ := json.Marshal(body)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Post(strings.TrimRight(pairHost, "/")+"/api/pair", "application/json", bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %s", resp.Status)
	}
	if !reusable {
		return code, nil
	}
	// A pair host that predates reusable invites parks it for two minutes, once.
	var parked struct {
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parked); err != nil {
		return "", err
	}
	if parked.ExpiresAt.Before(time.UnixMilli(invite.ExpiresAt).Add(-time.Minute)) {
		return "", fmt.Errorf("%s does not keep reusable invites", pairHost)
	}
	return code, nil
}

// pairReusable opens a reusable invite for days ("14d") or hours ("36h"), or
// ends it ("off"). Devices that joined with it stay paired either way.
func pairReusable(ctx context.Context, arg, pairHost string) error {
	if arg == "off" {
		if err := call(ctx, "pair.reusable_revoke", nil, nil); err != nil {
			return err
		}
		fmt.Println("Reusable invite ended. Devices that joined with it stay paired.")
		return nil
	}
	hours, err := reusableHours(arg)
	if err != nil {
		return err
	}
	var result struct {
		QR string `json:"qr"`
	}
	if err := call(ctx, "pair.reusable", map[string]int{"hours": hours}, &result); err != nil {
		return err
	}
	return printPairingQR(ctx, result.QR, pairHost, true)
}

func reusableHours(arg string) (int, error) {
	unit := map[string]int{"d": 24, "h": 1}[arg[len(arg)-1:]]
	n, err := strconv.Atoi(arg[:len(arg)-1])
	if unit == 0 || err != nil || n <= 0 {
		return 0, fmt.Errorf("--reusable takes days or hours, such as 14d or 36h, or off")
	}
	return n * unit, nil
}

// appClipCode renders the link as an App Clip Code SVG with the same encoder
// the site uses, run locally because no server may see the link. Empty when
// npx is unavailable or slow; the QR above stands alone.
func appClipCode(ctx context.Context, link string) []byte {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	svg, err := exec.CommandContext(ctx, "npx", "-y", "appclipcode@1.0.1", link).Output()
	if err != nil {
		return nil
	}
	return svg
}

// inlineImages reports whether the terminal speaks the iTerm2 inline image
// protocol (iTerm2, WezTerm, Ghostty ...). Elsewhere the QR above stands
// alone — Camera reads either.
func inlineImages() bool {
	switch {
	case os.Getenv("TERM_PROGRAM") == "iTerm.app",
		os.Getenv("TERM_PROGRAM") == "WezTerm",
		os.Getenv("TERM_PROGRAM") == "ghostty",
		os.Getenv("LC_TERMINAL") == "iTerm2":
		return true
	}
	return false
}

func inlineImage(data []byte, name string) string {
	return fmt.Sprintf("\x1b]1337;File=name=%s;size=%d;inline=1;width=24;preserveAspectRatio=1:%s\a",
		base64.StdEncoding.EncodeToString([]byte(name)), len(data),
		base64.StdEncoding.EncodeToString(data))
}

// terminalQR draws dark modules on a white field with an explicit quiet zone,
// whatever the terminal theme. Camera reads an inverted code slowly and a code
// with no border hardly at all; an in-app scanner forgives both, Camera does not.
func terminalQR(code *qrcode.QRCode) string {
	const quiet = 2 // modules of white on every side; two rows is four modules tall
	bits := code.Bitmap()
	// Bitmap already includes go-qrcode's own four-module border; pad to a
	// whole cell row and paint it explicitly so the theme cannot swallow it.
	rows := make([][]bool, 0, len(bits)+2*quiet)
	width := len(bits[0]) + 2*quiet
	blank := make([]bool, width)
	for i := 0; i < quiet; i++ {
		rows = append(rows, blank)
	}
	for _, r := range bits {
		row := make([]bool, 0, width)
		row = append(row, blank[:quiet]...)
		row = append(row, r...)
		row = append(row, blank[:quiet]...)
		rows = append(rows, row)
	}
	for i := 0; i < quiet; i++ {
		rows = append(rows, blank)
	}
	if len(rows)%2 == 1 {
		rows = append(rows, blank)
	}

	const on, off = "\x1b[30;107m", "\x1b[0m" // black on bright white
	var b strings.Builder
	for y := 0; y < len(rows); y += 2 {
		b.WriteString("  " + on)
		for x := 0; x < width; x++ {
			top, bottom := rows[y][x], rows[y+1][x]
			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString(off + "\n")
	}
	return b.String()
}

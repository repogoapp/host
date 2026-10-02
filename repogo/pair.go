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
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	"github.com/repogo/host/internal/device"
)

func printPairingQR(ctx context.Context, payload, pairHost string) error {
	invite, err := device.DecodeInvite(payload)
	if err != nil {
		return err
	}

	pairingCode, err := registerInvite(pairHost, payload)
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
	fmt.Printf("  Scan with Camera    expires in %v\n",
		time.Until(time.UnixMilli(invite.ExpiresAt)).Round(time.Second))
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

// registerInvite parks the invite on the pair host for its own lifetime,
// single use, and returns the code. The host gets only the code's hash
// and the invite sealed under the code; the code itself lives in the QR.
func registerInvite(pairHost, payload string) (string, error) {
	code := device.NewCode()
	sealed, err := device.SealInvite(code, payload)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]string{"lookup": device.CodeLookup(code), "sealed": sealed})
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Post(strings.TrimRight(pairHost, "/")+"/api/pair", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %s", resp.Status)
	}
	return code, nil
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

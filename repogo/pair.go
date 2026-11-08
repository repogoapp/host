package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	"github.com/repogoapp/host/internal/device"
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
	// The code rides in the fragment, which a browser never sends: a link that
	// opens on the web keeps it off the pair host and its logs. Apps older than
	// PairLink's `/p#` support read only `/<code>`.
	link := strings.TrimRight(pairHost, "/") + "/p#" + pairingCode

	code, err := qrcode.New(link, qrcode.Medium)
	if err != nil {
		return err
	}
	fmt.Print("\n" + terminalQR(code))
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

// terminalQR draws dark modules on a white field with an explicit quiet zone,
// whatever the terminal theme. Camera reads an inverted code slowly and a code
// with no border hardly at all; an in-app scanner forgives both, Camera does not.
func terminalQR(code *qrcode.QRCode) string {
	const quiet = 2 // modules of white on every side; two rows is four modules tall
	bits := code.Bitmap()
	// Terminal.app's block glyphs stop short of the line height and leave seams
	// Camera can't read past, so paint background colour instead: one module per
	// line, two spaces wide. Bitmap's own four-module border is the quiet zone.
	if os.Getenv("TERM_PROGRAM") == "Apple_Terminal" {
		const dark, light, off = "\x1b[40m", "\x1b[107m", "\x1b[0m"
		var b strings.Builder
		for _, row := range bits {
			b.WriteString("  ")
			for _, bit := range row {
				if bit {
					b.WriteString(dark + "  ")
				} else {
					b.WriteString(light + "  ")
				}
			}
			b.WriteString(off + "\n")
		}
		return b.String()
	}
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

package device

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ParkInvite parks an encoded invite on the pair host for its own lifetime and
// returns the code. The pair host gets only the code's hash and the invite
// sealed under the code; the code itself lives in the QR.
func ParkInvite(pairHost, payload string, invite Invite, reusable bool) (string, error) {
	code := NewCode()
	sealed, err := SealInvite(code, payload)
	if err != nil {
		return "", err
	}
	body := map[string]any{"lookup": CodeLookup(code), "sealed": sealed}
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

// CodeLink is the pairing link for a parked code. The code rides in the
// fragment, which a browser never sends, keeping it off the pair host's logs.
func CodeLink(pairHost, code string) string {
	return strings.TrimRight(pairHost, "/") + "/p#" + code
}

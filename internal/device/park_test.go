package device

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A one-time invite sends the body it always has; a reusable one asks for its
// lifetime and refuses a pair host that parks it for two minutes anyway.
func TestParkInviteReusable(t *testing.T) {
	var body map[string]any
	parkFor := 2 * time.Minute
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = nil
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(map[string]any{"expiresAt": time.Now().Add(parkFor)})
	}))
	defer srv.Close()
	invite := Invite{ExpiresAt: time.Now().Add(14 * 24 * time.Hour).UnixMilli()}

	if _, err := ParkInvite(srv.URL, "payload", invite, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["reusable"]; ok || len(body) != 2 {
		t.Fatalf("one-time body = %v, want lookup and sealed only", body)
	}

	if _, err := ParkInvite(srv.URL, "payload", invite, true); err == nil {
		t.Fatal("a two-minute parking was taken for a reusable invite")
	}
	if body["reusable"] != true || body["ttlMs"].(float64) < float64((14*24*time.Hour-time.Minute).Milliseconds()) {
		t.Fatalf("reusable body = %v", body)
	}

	parkFor = 14 * 24 * time.Hour
	if _, err := ParkInvite(srv.URL, "payload", invite, true); err != nil {
		t.Fatalf("reusable: %v", err)
	}
}

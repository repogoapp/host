package pushgate

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/jsonrpc"
)

// fcmRequest is a push to an FCM token carrying a fresh phone's grant for to.
func fcmRequest(t *testing.T, to device.ID, token string) PushRequest {
	t.Helper()
	phone, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}
	target := phone.GrantPush(to, device.PushTarget{Service: device.PushFCM, Token: token})
	return PushRequest{Token: token, Service: ServiceFCM, Payload: []byte(`{"kind":"agents"}`),
		Grant: PushGrant{Device: phone.ID, PublicKey: phone.Public, Signature: target.Grant, GrantedAt: target.GrantedAt}}
}

func noIntegrity(context.Context, []byte, []byte, time.Time, string, func() bool) error {
	return errors.New("no integrity")
}

// Anyone can sign a grant for an FCM token they learned, so an Android grant
// needs Play Integrity, or a phone the relay was told to trust.
func TestFCMNeedsIntegrityOrATrustedDevice(t *testing.T) {
	var apns, fcm atomic.Int32
	toAPNs := func(context.Context, PushRequest) error { apns.Add(1); return nil }
	toFCM := func(context.Context, PushRequest) error { fcm.Add(1); return nil }

	req := fcmRequest(t, host, "tok:1")
	if err := New(toAPNs, anyApp).WithFCM(toFCM, nil, nil).Send(t.Context(), host, req); code(err) != jsonrpc.CodeDenied {
		t.Fatalf("no integrity check: %v", err)
	}
	if err := New(toAPNs, anyApp).WithFCM(toFCM, noIntegrity, nil).Send(t.Context(), host, req); code(err) != jsonrpc.CodeDenied {
		t.Fatalf("failed integrity: %v", err)
	}
	if err := New(toAPNs, anyApp).WithFCM(toFCM, noIntegrity, []device.ID{req.Grant.Device}).Send(t.Context(), host, req); err != nil {
		t.Fatalf("trusted device: %v", err)
	}
	var gotToken string
	passes := func(_ context.Context, pub, _ []byte, _ time.Time, token string, _ func() bool) error {
		gotToken = token
		return nil
	}
	req.Integrity = "play-token"
	if err := New(toAPNs, anyApp).WithFCM(toFCM, passes, nil).Send(t.Context(), host, req); err != nil {
		t.Fatalf("passed integrity: %v", err)
	}
	if gotToken != "play-token" || fcm.Load() != 2 || apns.Load() != 0 {
		t.Fatalf("token %q, fcm %d, apns %d", gotToken, fcm.Load(), apns.Load())
	}
}

func TestFCMStillChecksTheSignature(t *testing.T) {
	req := fcmRequest(t, host, "tok:1")
	g := New(nil, nil).WithFCM(func(context.Context, PushRequest) error { return nil }, nil, []device.ID{req.Grant.Device})
	req.Token = "tok:2"
	if err := g.Send(t.Context(), host, req); code(err) != jsonrpc.CodeDenied {
		t.Fatalf("grant for another token: %v", err)
	}
	if err := g.Send(t.Context(), device.ID("00000000000000000000000000000002"), fcmRequest(t, host, "tok:1")); code(err) != jsonrpc.CodeDenied {
		t.Fatalf("grant for another host: %v", err)
	}
}

func TestEachServiceNeedsItsKey(t *testing.T) {
	apnsOnly := New(func(context.Context, PushRequest) error { return nil }, anyApp)
	if err := apnsOnly.Send(t.Context(), host, fcmRequest(t, host, "tok:1")); code(err) != jsonrpc.CodeUnavailable {
		t.Fatalf("fcm without a key: %v", err)
	}
	fcmOnly := New(nil, nil).WithFCM(func(context.Context, PushRequest) error { return nil }, noIntegrity, nil)
	if err := fcmOnly.Send(t.Context(), host, request(t, host, "abcd", time.Now())); code(err) != jsonrpc.CodeUnavailable {
		t.Fatalf("apns without a key: %v", err)
	}
	req := fcmRequest(t, host, "tok:1")
	req.Environment = EnvironmentSandbox
	if err := fcmOnly.Send(t.Context(), host, req); code(err) != jsonrpc.CodeInvalidParams {
		t.Fatalf("fcm with an environment: %v", err)
	}
	req.Service, req.Environment = "webpush", ""
	if err := fcmOnly.Send(t.Context(), host, req); code(err) != jsonrpc.CodeInvalidParams {
		t.Fatalf("unknown service: %v", err)
	}
}

// fakeGoogle answers the OAuth token and FCM send endpoints, recording each send.
func fakeGoogle(t *testing.T, sendStatus int, sendBody string) (*fcmSender, *[]map[string]any) {
	t.Helper()
	var sends []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			if !strings.HasPrefix(r.Form.Get("grant_type"), "urn:ietf:params:oauth:grant-type:jwt-bearer") || strings.Count(r.Form.Get("assertion"), ".") != 2 {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"access-1","expires_in":3600}`))
		case "/send":
			if r.Header.Get("Authorization") != "Bearer access-1" {
				http.Error(w, "{}", http.StatusUnauthorized)
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			sends = append(sends, body)
			w.WriteHeader(sendStatus)
			_, _ = w.Write([]byte(sendBody))
		}
	}))
	t.Cleanup(server.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &fcmSender{endpoint: server.URL + "/send", auth: &googleAuth{email: "relay@example.iam.gserviceaccount.com", key: key,
		scope: fcmScope, tokenURL: server.URL + "/token", client: server.Client()}}, &sends
}

func TestFCMSendsADataMessage(t *testing.T) {
	s, sends := fakeGoogle(t, http.StatusOK, `{"name":"projects/p/messages/1"}`)
	running := PushRequest{Token: "tok:1", Service: ServiceFCM, Priority: 5, Payload: []byte(`{"kind":"agents","state":"{\"working\":1}"}`)}
	if err := s.deliver(t.Context(), running); err != nil {
		t.Fatal(err)
	}
	if err := s.deliver(t.Context(), PushRequest{Token: "tok:1", Payload: []byte(`{"kind":"agents"}`)}); err != nil {
		t.Fatal(err)
	}
	message := (*sends)[0]["message"].(map[string]any)
	data := message["data"].(map[string]any)
	if message["token"] != "tok:1" || data["kind"] != "agents" || data["state"] != `{"working":1}` {
		t.Fatalf("message %v", message)
	}
	if _, ok := message["notification"]; ok {
		t.Error("a notification message never reaches the app's own drawing")
	}
	if android := message["android"].(map[string]any); android["priority"] != "normal" || android["ttl"] != "60s" {
		t.Errorf("running update %v", android)
	}
	if android := (*sends)[1]["message"].(map[string]any)["android"].(map[string]any); android["priority"] != "high" {
		t.Errorf("urgent push %v", android)
	}
}

func TestFCMUnregisteredIsADeadToken(t *testing.T) {
	s, _ := fakeGoogle(t, http.StatusNotFound,
		`{"error":{"status":"NOT_FOUND","details":[{"errorCode":"UNREGISTERED"}]}}`)
	err := s.deliver(t.Context(), PushRequest{Token: "tok:1", Payload: []byte(`{}`)})
	if !errors.Is(err, ErrUnregistered) {
		t.Fatalf("got %v", err)
	}
}

func TestFCMRefusesAPayloadThatIsNotStrings(t *testing.T) {
	s, sends := fakeGoogle(t, http.StatusOK, `{}`)
	if err := s.deliver(t.Context(), PushRequest{Token: "tok:1", Payload: []byte(`{"aps":{"alert":"hi"}}`)}); err == nil {
		t.Fatal("an aps body went to FCM")
	}
	if len(*sends) != 0 {
		t.Fatal("sent anyway")
	}
}

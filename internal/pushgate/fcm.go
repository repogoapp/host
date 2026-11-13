package pushgate

import (
	"bytes"
	"cmp"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"
)

// FCM's HTTP v1 API and the OAuth token it takes, from a service account.
const (
	fcmScope    = "https://www.googleapis.com/auth/firebase.messaging"
	playScope   = "https://www.googleapis.com/auth/playintegrity"
	googleToken = "https://oauth2.googleapis.com/token"
	fcmEndpoint = "https://fcm.googleapis.com/v1/projects/%s/messages:send"
)

// A normal-priority message is a Live Update's running state: a phone that
// gets it later than this has a newer one coming, so FCM drops it.
const fcmNormalTTL = 60 * time.Second

// serviceAccount is the part of a Google service account key FCM needs.
type serviceAccount struct {
	ProjectID   string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
}

// FCM delivers with the service account key in FCM_SERVICE_ACCOUNT (its JSON),
// to FCM_PROJECT_ID or the key's own project; nil with no key, so the relay
// refuses Android's pushes.
func FCM(log *slog.Logger) Deliver {
	account, auth := googleAccount(log, fcmScope)
	if auth == nil {
		return nil
	}
	s := &fcmSender{auth: auth, endpoint: fmt.Sprintf(fcmEndpoint, cmp.Or(os.Getenv("FCM_PROJECT_ID"), account.ProjectID))}
	return s.deliver
}

// googleAccount reads FCM_SERVICE_ACCOUNT, which signs in to FCM and to Play
// Integrity both; nil auth with no key or an unreadable one.
func googleAccount(log *slog.Logger, scope string) (serviceAccount, *googleAuth) {
	raw := os.Getenv("FCM_SERVICE_ACCOUNT")
	if raw == "" {
		return serviceAccount{}, nil
	}
	var account serviceAccount
	if err := json.Unmarshal([]byte(raw), &account); err != nil || account.ClientEmail == "" {
		log.Error("google: unreadable service account", "err", err)
		return serviceAccount{}, nil
	}
	key, err := rsaKey(account.PrivateKey)
	if err != nil {
		log.Error("google: unreadable private key", "err", err)
		return serviceAccount{}, nil
	}
	return account, &googleAuth{email: account.ClientEmail, key: key, scope: scope,
		tokenURL: googleToken, client: &http.Client{Timeout: 15 * time.Second}}
}

type fcmSender struct {
	auth     *googleAuth
	endpoint string
}

// googleAuth is a service account's OAuth token for one scope, minted from a
// signed JWT and kept until a minute before it lapses.
type googleAuth struct {
	email    string
	key      *rsa.PrivateKey
	scope    string
	tokenURL string
	client   *http.Client

	mu      sync.Mutex
	access  string
	expires time.Time
}

// deliver sends one data message. The payload is the message's data map: a
// flat JSON object of strings, which FCM requires.
func (s *fcmSender) deliver(ctx context.Context, req PushRequest) error {
	var data map[string]string
	if err := json.Unmarshal(req.Payload, &data); err != nil {
		return fmt.Errorf("fcm: payload must be a JSON object of strings: %w", err)
	}
	android := map[string]any{"priority": "high"}
	if req.Priority == 5 {
		android = map[string]any{"priority": "normal", "ttl": strconv.Itoa(int(fcmNormalTTL.Seconds())) + "s"}
	}
	body, _ := json.Marshal(map[string]any{"message": map[string]any{
		"token": req.Token, "data": data, "android": android,
	}})
	access, err := s.auth.token(ctx)
	if err != nil {
		return err
	}
	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	httpReq.Header.Set("Authorization", "Bearer "+access)
	httpReq.Header.Set("Content-Type", "application/json")
	res, err := s.auth.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("fcm: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		return nil
	}
	reason := fcmReason(res.Body)
	if res.StatusCode == http.StatusNotFound || reason == "UNREGISTERED" {
		// The reason travels to the host, which logs why it forgot the token.
		return fmt.Errorf("%w: %d %s", ErrUnregistered, res.StatusCode, reason)
	}
	if res.StatusCode == http.StatusUnauthorized {
		s.auth.forget()
	}
	return fmt.Errorf("fcm: %d %s", res.StatusCode, reason)
}

// fcmReason is the FCM error code in a failed answer, else its status.
func fcmReason(body io.Reader) string {
	var answer struct {
		Error struct {
			Status  string `json:"status"`
			Details []struct {
				ErrorCode string `json:"errorCode"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(body, 64<<10)).Decode(&answer)
	for _, d := range answer.Error.Details {
		if d.ErrorCode != "" {
			return d.ErrorCode
		}
	}
	return answer.Error.Status
}

// forget drops a token Google refused, so the next call mints another.
func (s *googleAuth) forget() {
	s.mu.Lock()
	s.access = ""
	s.mu.Unlock()
}

// token is the account's OAuth token for its scope.
func (s *googleAuth) token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.access != "" && now.Before(s.expires.Add(-time.Minute)) {
		return s.access, nil
	}
	assertion, err := s.jwt(now)
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, bytes.NewBufferString(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("google: token: %w", err)
	}
	defer res.Body.Close()
	var answer struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&answer); err != nil || answer.AccessToken == "" {
		return "", fmt.Errorf("google: token: %d %s", res.StatusCode, answer.Error)
	}
	s.access, s.expires = answer.AccessToken, now.Add(time.Duration(answer.ExpiresIn)*time.Second)
	return s.access, nil
}

func (s *googleAuth) jwt(now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iss": s.email, "scope": s.scope, "aud": s.tokenURL,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	unsigned := header + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(nil, s.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("google: sign: %w", err)
	}
	return unsigned + "." + enc.EncodeToString(sig), nil
}

// rsaKey reads a service account's PEM private key (PKCS #8, or PKCS #1).
func rsaKey(pemKey string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemKey))
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
		return nil, errors.New("not an RSA key")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

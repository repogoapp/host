package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Avatar fetches the signed-in account's picture on the host, which holds the
// GitHub identity. Content-addressed so `ifNoneMatch` makes every later check
// a reply with no bytes.
func (s *Service) Avatar(ctx context.Context, ifNoneMatch string) (Avatar, error) {
	ok, _, account := signedIn(ctx, "gh")
	if !ok || account.AvatarURL == "" {
		return Avatar{}, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, account.AvatarURL, nil)
	if err != nil {
		return Avatar{}, err
	}
	res, err := avatarClient.Do(req)
	if err != nil {
		return Avatar{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Avatar{}, fmt.Errorf("github: avatar: %s", res.Status)
	}

	// Bounded: an avatar is a few kilobytes and anything claiming to be
	// megabytes is not one.
	body, err := io.ReadAll(io.LimitReader(res.Body, maxAvatarBytes))
	if err != nil {
		return Avatar{}, err
	}

	sum := sha256.Sum256(body)
	out := Avatar{
		Login:       account.Login,
		URL:         account.AvatarURL,
		ContentHash: hex.EncodeToString(sum[:]),
		// Reported by the server rather than guessed from the URL: GitHub serves JPEG
		// from paths ending `.png`.
		ContentType: res.Header.Get("Content-Type"),
	}
	if ifNoneMatch != "" && ifNoneMatch == out.ContentHash {
		out.NotModified = true
		return out, nil
	}
	out.Bytes = body
	return out, nil
}

// Avatar is the signed-in account's picture, addressed by its content.
type Avatar struct {
	Login       string `json:"login,omitempty"`
	URL         string `json:"url,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Bytes       []byte `json:"bytes,omitempty"`
	NotModified bool   `json:"not_modified,omitempty"`
}

const maxAvatarBytes = 1 << 20

// avatarClient is separate from anything else: one short timeout, no redirect
// surprises beyond the one GitHub always issues.
var avatarClient = &http.Client{Timeout: 20 * time.Second}

// Package account links this host to one RepoGo account. The phone asks
// repogo.app for a nonce, the host signs it together with the account's uid,
// and repogo.app records the host as that account's. The host keeps
// the uid it signed for and refuses another until the user releases it here.
package account

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/repogo/host/internal/apphome"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/errkind"
)

var (
	ErrInvalid = errkind.New(errkind.Invalid, "invalid claim")
	// ErrClaimedElsewhere is a second account asking; only the user at the
	// machine can release the first (`repogo account release`).
	ErrClaimedElsewhere = errkind.New(errkind.Denied, "this environment is linked to another RepoGo account; run repogo account release on it first")
)

// Firebase uids are at most 128 characters; the separator never appears in one.
var (
	uidPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	noncePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
)

// Proof is what the phone forwards to repogo.app's POST /api/hosts/claim.
type Proof struct {
	HostID    device.ID `json:"host_id"`
	PublicKey []byte    `json:"public_key"`
	Signature []byte    `json:"signature"`
}

// Link is the account this host is claimed by.
type Link struct {
	UID       string `json:"uid"`
	ClaimedAt int64  `json:"claimed_at"`
}

type Service struct {
	path string
	id   *device.Identity

	mu   sync.Mutex
	link Link
}

// Open loads path.
func Open(path string, id *device.Identity) (*Service, error) {
	s := &Service{path: path, id: id}
	if _, err := apphome.ReadJSON(path, &s.link); err != nil {
		return nil, err
	}
	return s, nil
}

// Message is what the host signs; repogo.app's server/hostClaim.ts builds the same bytes.
func Message(uid, nonce string, host device.ID) []byte {
	return []byte("repogo-host-claim/1|" + uid + "|" + nonce + "|" + string(host))
}

// Claim signs for uid, remembering it. The same uid may claim again, since a
// phone re-claims after repogo.app forgets a host.
func (s *Service) Claim(uid, nonce string) (Proof, error) {
	if !uidPattern.MatchString(uid) || !noncePattern.MatchString(nonce) {
		return Proof{}, fmt.Errorf("%w: uid or nonce is malformed", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.link.UID != "" && s.link.UID != uid {
		return Proof{}, ErrClaimedElsewhere
	}
	if s.link.UID == "" {
		next := Link{UID: uid, ClaimedAt: time.Now().UnixMilli()}
		if err := s.save(next); err != nil {
			return Proof{}, err
		}
		s.link = next
	}
	return Proof{HostID: s.id.ID, PublicKey: s.id.Public, Signature: s.id.Sign(Message(uid, nonce, s.id.ID))}, nil
}

// Current is the linked account, zero when unclaimed.
func (s *Service) Current() Link {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.link
}

// Release forgets the account so another may claim this host. repogo.app keeps
// its record until the old account releases the host there too.
func (s *Service) Release() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.link = Link{}
	return nil
}

func (s *Service) save(l Link) error {
	return apphome.WriteJSON(s.path, l, 0o600)
}

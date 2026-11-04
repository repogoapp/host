package device

import (
	"errors"
	"path/filepath"
	"testing"
)

func pairedStore(t *testing.T) (*Store, *Identity) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "device.json"))
	if err != nil {
		t.Fatal(err)
	}
	phone, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Add(Peer{ID: phone.ID, Public: phone.Public, Label: "iPhone",
		Push: &PushTarget{Token: "aa"}}); err != nil {
		t.Fatal(err)
	}
	return store, phone
}

func TestRotateMovesTheDeviceToItsSecureEnclaveKey(t *testing.T) {
	store, phone := pairedStore(t)
	se := newP256Phone(t)
	msg, err := RotateMessage(store.Identity().ID, phone.ID, se.public)
	if err != nil {
		t.Fatal(err)
	}
	next, err := store.Rotate(phone.ID, se.public, phone.Sign(msg), se.sign(t, msg))
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != IDFor(se.public) || next.Label != "iPhone" || next.Push != nil {
		t.Fatalf("rotated peer = %+v; want the new id, the label kept, push tokens cleared", next)
	}
	if _, err := store.Peer(phone.ID); !errors.Is(err, ErrUnknownDevice) {
		t.Fatalf("the old id is still paired: %v", err)
	}
	// The old key is refused at once; the new one is accepted.
	login := []byte("login")
	if store.Verify(phone.ID, login, phone.Sign(login)) == nil {
		t.Fatal("the old key still signs in")
	}
	if err := store.Verify(next.ID, login, se.sign(t, login)); err != nil {
		t.Fatalf("the new key does not sign in: %v", err)
	}
	// Saved: a reopened store has the rotated device.
	reopened, err := Open(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Peer(next.ID); err != nil {
		t.Fatalf("rotation was not saved: %v", err)
	}
}

func TestRotateNeedsBothKeysAndThisHost(t *testing.T) {
	store, phone := pairedStore(t)
	se := newP256Phone(t)
	msg, _ := RotateMessage(store.Identity().ID, phone.ID, se.public)
	other, _ := Generate()
	elsewhere, _ := RotateMessage(other.ID, phone.ID, se.public)
	cases := map[string][2][]byte{
		"no new-key signature":    {phone.Sign(msg), nil},
		"no old-key signature":    {nil, se.sign(t, msg)},
		"signed for another host": {phone.Sign(elsewhere), se.sign(t, elsewhere)},
		"old key signs twice":     {phone.Sign(msg), phone.Sign(msg)},
	}
	for name, sigs := range cases {
		if _, err := store.Rotate(phone.ID, se.public, sigs[0], sigs[1]); !errors.Is(err, ErrRotateProof) {
			t.Errorf("%s: err = %v, want ErrRotateProof", name, err)
		}
	}
	if _, err := store.Peer(phone.ID); err != nil {
		t.Fatalf("a refused rotation changed the store: %v", err)
	}
}

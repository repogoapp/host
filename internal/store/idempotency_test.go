package store

import (
	"bytes"
	"testing"
)

func TestIdempotencyKeysAndTurnChats(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, _, ok, err := db.IdempotencyKey("device:a", "chats.send", "k1"); ok || err != nil {
		t.Fatalf("before a save: %v %v", ok, err)
	}
	if err := db.SaveIdempotencyKey("device:a", "chats.send", "k1", []byte{1}, []byte(`{"turn_id":"t1"}`), 10); err != nil {
		t.Fatal(err)
	}
	hash, result, ok, err := db.IdempotencyKey("device:a", "chats.send", "k1")
	if err != nil || !ok || !bytes.Equal(hash, []byte{1}) || string(result) != `{"turn_id":"t1"}` {
		t.Fatalf("after a save: %v %q %v %v", hash, result, ok, err)
	}
	if _, _, ok, _ := db.IdempotencyKey("device:b", "chats.send", "k1"); ok {
		t.Fatal("another scope read the key")
	}
	if err := db.SetTurnChat("t1", "claude:s1", 10); err != nil {
		t.Fatal(err)
	}
	if chat, err := db.TurnChat("t1"); err != nil || chat != "claude:s1" {
		t.Fatalf("turn chat: %q %v", chat, err)
	}
	if err := db.SaveIdempotencyKey("device:a", "chats.send", "k2", []byte{2}, []byte(`{}`), 30); err != nil {
		t.Fatal(err)
	}
	if err := db.PruneIdempotency(20); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := db.IdempotencyKey("device:a", "chats.send", "k1"); ok {
		t.Fatal("an old key survived the prune")
	}
	if _, _, ok, _ := db.IdempotencyKey("device:a", "chats.send", "k2"); !ok {
		t.Fatal("a new key was pruned")
	}
	if chat, _ := db.TurnChat("t1"); chat != "" {
		t.Fatalf("an old turn chat survived the prune: %q", chat)
	}
	if err := db.DeleteIdempotencyKey("device:a", "chats.send", "k2"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := db.IdempotencyKey("device:a", "chats.send", "k2"); ok {
		t.Fatal("a deleted key is still kept")
	}
}

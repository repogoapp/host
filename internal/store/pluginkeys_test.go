package store

import "testing"

func TestPluginKeysReplaceAndDelete(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if key, at, err := db.PluginKey("shop"); err != nil || key != "" || at != 0 {
		t.Fatalf("before a key: %q %d %v", key, at, err)
	}
	for i, key := range []string{"rgk_one", "rgk_two"} {
		if err := db.SetPluginKey("shop", key, int64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if key, at, err := db.PluginKey("shop"); err != nil || key != "rgk_two" || at != 2 {
		t.Fatalf("after replacing: %q %d %v", key, at, err)
	}
	if err := db.DeletePluginKey("shop"); err != nil {
		t.Fatal(err)
	}
	if key, _, _ := db.PluginKey("shop"); key != "" {
		t.Fatalf("after delete: %q", key)
	}
	if err := db.DeletePluginKey("shop"); err != nil {
		t.Fatalf("deleting a missing key: %v", err)
	}
}

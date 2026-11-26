package plugins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPluginIconReadsEachDeclaredForm(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "icon.png"), []byte{0x89, 'P', 'N', 'G'}, 0o600); err != nil {
		t.Fatal(err)
	}
	symbol := pluginIcon(json.RawMessage(`{"symbol":"shippingbox.fill","android":"deployed_code","web":"package","color":"green"}`), dir)
	if symbol == nil || *symbol != (Icon{Symbol: "shippingbox.fill", Android: "deployed_code", Web: "package", Color: "green"}) {
		t.Fatalf("a symbol: %+v", symbol)
	}
	if got := pluginIcon(json.RawMessage(`{"site":"vercel.com"}`), dir); got == nil || got.Site != "vercel.com" {
		t.Fatalf("a site: %+v", got)
	}
	if got := pluginIcon(json.RawMessage(`{"image":"icon.png"}`), dir); got == nil || !strings.HasPrefix(got.Image, "data:image/png;base64,") {
		t.Fatalf("a file in the folder: %+v", got)
	}
	if got := pluginIcon(json.RawMessage(`{"image":"https://cdn.example.com/logo.png"}`), dir); got == nil || got.Image != "https://cdn.example.com/logo.png" {
		t.Fatalf("an https URL: %+v", got)
	}
	for _, raw := range []string{`null`, `"icon.png"`, `{}`, `{"image":"../secret.png"}`, `{"image":"icon.gif"}`, `{"image":"missing.png"}`, `{"image":"http://plain.example.com/a.png"}`} {
		if got := pluginIcon(json.RawMessage(raw), dir); got != nil {
			t.Fatalf("%s should have no icon, got %+v", raw, got)
		}
	}
}

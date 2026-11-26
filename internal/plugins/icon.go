package plugins

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Icon is how a device draws a plugin, as a card Item draws its tile: a
// symbol on a coloured tile (Symbol on iOS, Android and Web their own names,
// Color one of the cards' named colours), a site's favicon, or a picture.
type Icon struct {
	Symbol  string `json:"symbol,omitempty"`
	Android string `json:"android,omitempty"`
	Web     string `json:"web,omitempty"`
	Color   string `json:"color,omitempty"`
	// Site is a site whose favicon is the tile, such as vercel.com.
	Site string `json:"site,omitempty"`
	// Image is the picture that is the tile: an https URL as declared, or a
	// file from the plugin's folder as a data URL, so a device needs no access
	// to the folder.
	Image string `json:"image,omitempty"`
}

// iconMaxBytes keeps a list of plugins small: an icon is drawn at 30 pt.
const iconMaxBytes = 64 << 10

var iconTypes = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp"}

// pluginIcon reads definePlugin's icon. Nil for none; an image file outside
// the folder, of another type or over iconMaxBytes, or an image URL that
// isn't https, is left out.
func pluginIcon(raw json.RawMessage, dir string) *Icon {
	var icon Icon
	if json.Unmarshal(raw, &icon) != nil {
		return nil
	}
	switch {
	case strings.HasPrefix(icon.Image, "https://"):
	case icon.Image != "":
		icon.Image = iconFile(icon.Image, dir)
	}
	if icon == (Icon{}) {
		return nil
	}
	return &icon
}

// iconFile is a picture in the plugin's folder as a data URL, "" when it can't be.
func iconFile(file, dir string) string {
	path := filepath.Join(dir, filepath.FromSlash(file))
	if rel, err := filepath.Rel(dir, path); err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	mime := iconTypes[strings.ToLower(filepath.Ext(path))]
	info, err := os.Stat(path)
	if mime == "" || err != nil || info.Size() > iconMaxBytes {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

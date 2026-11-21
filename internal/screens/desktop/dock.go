package desktop

import (
	"bytes"
	"encoding/xml"
	"strings"
)

func parseDock(raw []byte) ([]App, error) {
	var plist struct {
		Dict node `xml:"dict"`
	}
	if err := xml.NewDecoder(bytes.NewReader(raw)).Decode(&plist); err != nil {
		return nil, err
	}
	tiles, _ := plist.Dict.get("persistent-apps")
	var out []App
	for _, tile := range tiles.Children {
		data, _ := tile.get("tile-data")
		label, _ := data.get("file-label")
		id, _ := data.get("bundle-identifier")
		bundle := strings.TrimSpace(id.Text)
		if bundle == "" || bundle == finder {
			continue
		}
		out = append(out, App{Bundle: bundle, Name: strings.TrimSpace(label.Text)})
	}
	return out, nil
}

// node is any plist element: <dict>, <array>, <string>, <data>.
type node struct {
	XMLName  xml.Name
	Text     string `xml:",chardata"`
	Children []node `xml:",any"`
}

// get returns the value for key in a <dict>.
func (n node) get(key string) (node, bool) {
	for i := 0; i+1 < len(n.Children); i++ {
		if c := n.Children[i]; c.XMLName.Local == "key" && c.Text == key {
			return n.Children[i+1], true
		}
	}
	return node{}, false
}

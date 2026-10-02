package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/repogo/host/internal/apphome"
)

// A phone has no way onto this disk but the message itself, so the bytes come
// inline. The relay bounds one message at 8 MiB; these bounds leave room for
// base64's extra third and the prompt around it.
const (
	maxUploads      = 10
	MaxUploadBytes  = 4 << 20
	maxUploadsBytes = 5 << 20
)

// attachmentsDir is under the state directory, never the project: `git status`
// should not change because a screenshot was sent.
const attachmentsDir = "attachments"

// AttachmentsDir is where every uploaded file lives, one directory per message.
// The phone reads them back from here to draw a sent message's tiles.
func AttachmentsDir() (string, error) { return apphome.Path(attachmentsDir) }

// Upload is one file sent with a prompt. Data is base64 on the wire, which
// encoding/json undoes.
type Upload struct {
	Name     string `json:"name"`
	MimeType string `json:"mime_type"`
	Data     []byte `json:"data"`
}

// SaveUploads writes each file under AttachmentsDir.
func SaveUploads(in []Upload) ([]Attachment, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > maxUploads {
		return nil, fmt.Errorf("%w: at most %d attachments per message", ErrInvalidTurn, maxUploads)
	}
	var total int
	for _, a := range in {
		if len(a.Data) == 0 {
			return nil, fmt.Errorf("%w: attachment %q is empty", ErrInvalidTurn, a.Name)
		}
		if len(a.Data) > MaxUploadBytes {
			return nil, fmt.Errorf("%w: attachment %q is larger than %d MiB", ErrInvalidTurn, a.Name, MaxUploadBytes>>20)
		}
		total += len(a.Data)
	}
	if total > maxUploadsBytes {
		return nil, fmt.Errorf("%w: attachments total more than %d MiB", ErrInvalidTurn, maxUploadsBytes>>20)
	}

	// One directory per message, so two sends of "screenshot.png" keep both.
	batch := uuid.NewString()
	out := make([]Attachment, 0, len(in))
	for _, a := range in {
		name := uploadName(a.Name, a.MimeType)
		path, err := apphome.MkdirAll(attachmentsDir, batch, name)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, a.Data, 0o600); err != nil {
			return nil, err
		}
		mime := strings.TrimSpace(a.MimeType)
		if mime == "" {
			mime = "application/octet-stream"
		}
		out = append(out, Attachment{Name: name, MimeType: mime, Path: path})
	}
	return out, nil
}

// uploadName keeps the name the user saw, minus anything that would move the
// file out of its directory or make a shell choke on the path.
func uploadName(name, mimeType string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || unicode.IsControl(r) {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		name = "attachment" + extensionFor(mimeType)
	}
	return name
}

func extensionFor(mimeType string) string {
	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/heic":
		return ".heic"
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	}
	return ""
}

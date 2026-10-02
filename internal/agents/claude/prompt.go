package claude

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/google/uuid"
	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/claudecode"
)

var mcpCommand = regexp.MustCompile(`^/mcp:([^:\s]+):(\S+)(?:\s(.*))?$`)

func userMessage(req agent.TurnRequest, sessionID string) claudecode.UserMessage {
	m := claudecode.UserMessage{Type: "user", UUID: uuid.NewString(), SessionID: sessionID}
	m.Origin.Kind = "human"
	m.Message.Role = "user"
	if req.Prompt != "" {
		prompt := req.Prompt
		if match := mcpCommand.FindStringSubmatch(prompt); match != nil {
			prompt = "/" + match[1] + ":" + match[2] + " (MCP)"
			if match[3] != "" {
				prompt += " " + match[3]
			}
		}
		m.Message.Content = append(m.Message.Content, claudecode.ContentBlock{Type: "text", Text: prompt})
	}
	for _, attachment := range req.Attachments {
		if attachment.IsImage() {
			data, err := os.ReadFile(attachment.Path)
			if err == nil {
				m.Message.Content = append(m.Message.Content, claudecode.ContentBlock{Type: "image", Source: &claudecode.ImageSource{Type: "base64", MediaType: attachment.MimeType, Data: base64.StdEncoding.EncodeToString(data)}})
			}
		}
		m.Message.Content = append(m.Message.Content, claudecode.ContentBlock{Type: "text", Text: fmt.Sprintf("[@%s](file://%s)", filepath.Base(attachment.Path), attachment.Path)})
	}
	return m
}

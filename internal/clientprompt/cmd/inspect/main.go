// Command inspect describes the prompt assembled from the current source.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/repogoapp/host/internal/clientprompt"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/repogomcp"
)

type request struct {
	Features map[string]int       `json:"features"`
	Plugins  clientprompt.Plugins `json:"plugins"`
	Servers  []string             `json:"servers"`
	Tools    bool                 `json:"tools"`
	Child    bool                 `json:"child"`
}

type result struct {
	Sections []clientprompt.Section `json:"sections"`
	Text     string                 `json:"text"`
}

func main() {
	if err := inspect(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func inspect(input io.Reader, output io.Writer) error {
	var req request
	decoder := json.NewDecoder(io.LimitReader(input, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return err
	}
	if req.Plugins < clientprompt.NoPlugins || req.Plugins > clientprompt.HasPlugins {
		return fmt.Errorf("invalid plugin availability: %d", req.Plugins)
	}
	client := device.Client{Features: req.Features}
	chats := repogomcp.ChatsPrompt(req.Child)
	sections := append([]clientprompt.Section{{
		ID: "agents", Title: "Other agents", File: "internal/repogomcp/chatsprompt.go",
		Included: true, Reason: "RepoGo tools enabled; instructions follow the chat's parent status.", Text: chats,
	}}, clientprompt.Sections(client, req.Plugins, req.Servers)...)
	text := ""
	if req.Tools {
		text = chats + clientprompt.For(client, req.Plugins, req.Servers)
	} else {
		for i := range sections {
			sections[i].Included = false
			sections[i].Reason = "RepoGo tools disabled for this environment."
		}
	}
	return json.NewEncoder(output).Encode(result{Sections: sections, Text: text})
}

package cursor

import (
	"context"
	"os"
	"time"

	"github.com/repogo/host/internal/agentcatalog"
	"github.com/repogo/host/internal/cursoragent"
)

func (p *Provider) Catalog(ctx context.Context) agentcatalog.Catalog {
	if err := p.Available(); err != nil {
		return agentcatalog.Unavailable(err.Error())
	}
	// A neutral project prevents model discovery from loading project instructions or MCP servers.
	cwd, err := os.MkdirTemp("", "repogo-cursor-catalog-")
	if err != nil {
		return agentcatalog.Unavailable(err.Error())
	}
	defer os.RemoveAll(cwd)
	c, err := cursoragent.Start(ctx, cursoragent.Options{Executable: p.executable(), Cwd: cwd, Env: p.environment()}, cursoragent.Handlers{Notification: func(cursoragent.Notification) error { return nil }})
	if err != nil {
		return agentcatalog.Unavailable(err.Error())
	}
	defer c.Close()
	init, err := c.Initialize(ctx)
	if err != nil {
		return agentcatalog.Unavailable(err.Error())
	}
	state, err := c.Open(ctx, cursoragent.SessionParams{Cwd: cwd})
	if err != nil {
		return agentcatalog.Unavailable(err.Error())
	}
	return catalog(state, init.AgentCapabilities.PromptCapabilities.Image)
}
func catalog(state cursoragent.Session, images bool) agentcatalog.Catalog {
	out := agentcatalog.Catalog{Available: true, Detail: "live from Cursor ACP session/new", CapturedAtMS: time.Now().UnixMilli(), DefaultModel: state.Models.CurrentModelID, Models: []agentcatalog.Model{}, Modes: []agentcatalog.Choice{}, PermissionModes: []agentcatalog.Choice{}}
	for _, m := range state.Models.AvailableModels {
		modalities := []string{"text"}
		if images {
			modalities = append(modalities, "image")
		}
		out.Models = append(out.Models, agentcatalog.Model{ID: m.ModelID, Name: m.Name, Description: m.Description, Default: m.ModelID == state.Models.CurrentModelID, Efforts: []agentcatalog.Choice{}, Modalities: modalities})
	}
	if len(out.Models) == 0 {
		return agentcatalog.Unavailable("Cursor returned no models")
	}
	for _, m := range state.Modes.AvailableModes {
		out.Modes = append(out.Modes, agentcatalog.Choice{Value: m.ID, Name: m.Name, Description: m.Description})
	}
	return out
}

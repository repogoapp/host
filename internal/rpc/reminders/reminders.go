// Package reminders is the `reminders.*` methods: one-shot notifications to
// the phones at a set time, which agents set through RepoGo's MCP; the
// firing lives in internal/reminder.
package reminders

import (
	"context"

	"github.com/repogoapp/host/internal/reminder"
	"github.com/repogoapp/host/internal/rpc"
	"github.com/repogoapp/host/internal/store"
)

type Deps struct {
	Reminders *reminder.Reminders
}

type ListParams struct {
	Path string `json:"path,omitempty" doc:"Only this project's; every reminder when left out"`
}

type ListResult struct {
	Reminders []store.Reminder `json:"reminders"`
}

type DeleteParams struct {
	ID string `json:"id" doc:"The reminder's id, as reminders_list gives it"`
}

func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "reminders.list", d.list, rpc.Tool("The pending reminders on this environment, soonest first."), rpc.Read)
	rpc.Add(r, "reminders.create", d.create, rpc.Tool("Notify the user's phones at a set time; a tap opens "+
		"this chat. Set one only when the user asks for it or agrees to it."), rpc.Write)
	rpc.Add(r, "reminders.delete", d.delete, rpc.Tool("Cancel a pending reminder."), rpc.Write)
}

func (d Deps) list(_ context.Context, _ rpc.Caller, a ListParams) (ListResult, error) {
	rows, err := d.Reminders.List(a.Path)
	return ListResult{Reminders: rows}, err
}

// create keeps who set it: the user, or the chat whose agent called, whose
// own chat a tap opens unless it names another.
func (d Deps) create(_ context.Context, c rpc.Caller, a reminder.New) (store.Reminder, error) {
	setBy := "user"
	if c.Agent != nil {
		setBy = c.Agent.ChatID
		if a.ChatID == "" {
			a.ChatID = c.Agent.ChatID
		}
	}
	return d.Reminders.Create(a, setBy)
}

func (d Deps) delete(_ context.Context, _ rpc.Caller, a DeleteParams) (rpc.Ack, error) {
	return rpc.OK, d.Reminders.Delete(a.ID)
}

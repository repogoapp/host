// Package screens is the `screens.*` methods, and the /stream request
// that shows one: this machine's simulators, emulators and its display.
// The tools are driven in internal/screens.
package screens

import (
	"context"
	"encoding/json"

	"github.com/repogoapp/host/internal/rpc"
	core "github.com/repogoapp/host/internal/screens"
	"github.com/repogoapp/host/internal/screens/capture"
)

type ListResult struct {
	Screens []core.Screen `json:"screens" wire:"array"`
}

type IDParams struct {
	ID string `json:"id"`
}

// StreamRequest is a /stream socket's first message: whose screen to send,
// and for a display the account password the phone keeps; never logged.
type StreamRequest struct {
	Screen   string `json:"screen"`
	Password string `json:"password,omitempty"`
}

func Register(r *rpc.Router) {
	// list is every simulator and emulator here, iOS first; a platform
	// without its tools lists none.
	rpc.Add(r, "screens.list", list)
	// boot answers once the device is ready; an Android cold boot takes up to a minute.
	rpc.Add(r, "screens.boot", boot, rpc.Detached)
	rpc.Add(r, "screens.shutdown", shutdown)
}

func list(ctx context.Context, _ rpc.Caller, _ rpc.None) (ListResult, error) {
	screens, err := core.List(ctx)
	if screens == nil {
		screens = []core.Screen{}
	}
	// One platform's failure still lists the other's.
	if len(screens) > 0 {
		err = nil
	}
	return ListResult{Screens: screens}, err
}

func boot(ctx context.Context, _ rpc.Caller, a IDParams) (rpc.Ack, error) {
	return rpc.OK, core.Boot(ctx, a.ID)
}

func shutdown(ctx context.Context, _ rpc.Caller, a IDParams) (rpc.Ack, error) {
	return rpc.OK, core.Shutdown(ctx, a.ID)
}

// Stream serves a /stream socket: the screen of the simulator its request
// names, and the touches, buttons and actions (capture.Input) its later
// messages carry; reply sends the device's controls, state and results.
func Stream(ctx context.Context, _ rpc.Caller, request []byte, send func([]byte) error, reply func(any) error,
	input <-chan []byte) error {
	var a StreamRequest
	if err := json.Unmarshal(request, &a); err != nil || a.Screen == "" {
		return rpc.ErrInvalidParams
	}
	parsed := make(chan capture.Input)
	go func() {
		defer close(parsed)
		for b := range input {
			var in capture.Input
			if json.Unmarshal(b, &in) != nil {
				continue
			}
			select {
			case parsed <- in:
			case <-ctx.Done():
				return
			}
		}
	}()
	return core.Stream(ctx, a.Screen, a.Password, send, reply, parsed)
}

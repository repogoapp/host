package push

import (
	"cmp"
	"context"
	"log/slog"

	"github.com/repogoapp/host/internal/pushgate"
	"github.com/repogoapp/host/internal/release"
)

// Logged is s in a dev build with a line for every push it sends: an alert
// or a Live Activity, to which phone, APNs or FCM, and whether the relay took
// it. It's for seeing how often the host pushes; a release host logs none.
func Logged(s Sender, log *slog.Logger) Sender {
	if !release.IsDev() {
		return s
	}
	return loggedSender{s, log}
}

type loggedSender struct {
	Sender
	log *slog.Logger
}

func (s loggedSender) Push(ctx context.Context, req pushgate.PushRequest) error {
	err := s.Sender.Push(ctx, req)
	s.log.Info("push: sent", "device", req.Grant.Device, "service", cmp.Or(req.Service, "apns"),
		"push_type", cmp.Or(req.PushType, "alert"), "environment", req.Environment,
		"priority", req.Priority, "bytes", len(req.Payload), "err", err)
	return err
}

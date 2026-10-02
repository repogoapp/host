package fly

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Secret is one of an app's secrets by name; Fly never returns a value.
type Secret struct {
	Name string `json:"name"`
}

type inputRunner func(ctx context.Context, args []string, stdin []byte) ([]byte, error)

func ListSecrets(ctx context.Context, app string) ([]Secret, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return listSecretsWith(ctx, app, runCLIInput)
}

func listSecretsWith(ctx context.Context, app string, run inputRunner) ([]Secret, error) {
	out, err := run(ctx, []string{"secrets", "list", "-a", app, "--json"}, nil)
	if err != nil {
		return nil, err
	}
	secrets := []Secret{}
	if err := json.Unmarshal(out, &secrets); err != nil {
		return nil, fmt.Errorf("decode secrets: %w", err)
	}
	return secrets, nil
}

// SetSecret reads the value from stdin (NAME=-). Without deploy the secret is
// staged and the machines keep running until the next deploy.
func SetSecret(ctx context.Context, app, name, value string, deploy bool) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return setSecretWith(ctx, app, name, value, deploy, runCLIInput)
}

func setSecretWith(ctx context.Context, app, name, value string, deploy bool, run inputRunner) error {
	_, err := run(ctx, []string{"secrets", "set", name + "=-", "-a", app, deployFlag(deploy)}, []byte(value))
	return err
}

func UnsetSecret(ctx context.Context, app, name string, deploy bool) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return unsetSecretWith(ctx, app, name, deploy, runCLIInput)
}

func unsetSecretWith(ctx context.Context, app, name string, deploy bool, run inputRunner) error {
	_, err := run(ctx, []string{"secrets", "unset", name, "-a", app, deployFlag(deploy)}, nil)
	return err
}

// deployFlag restarts the machines in the background, or stages the change
// for the next deploy.
func deployFlag(deploy bool) string {
	if deploy {
		return "--detach"
	}
	return "--stage"
}

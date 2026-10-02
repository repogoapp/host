package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Secret is one of a Worker's secrets by name; Cloudflare never returns a value.
type Secret struct {
	Name string `json:"name"`
}

type inputRunner func(ctx context.Context, env []string, args []string, stdin []byte) ([]byte, error)

func ListSecrets(ctx context.Context, worker, accountID string) ([]Secret, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return listSecretsWith(ctx, worker, accountID, runCLIInput)
}

func listSecretsWith(ctx context.Context, worker, accountID string, run inputRunner) ([]Secret, error) {
	env, err := accountEnv(ctx, accountID, run)
	if err != nil {
		return nil, err
	}
	out, err := run(ctx, env, []string{"secret", "list", "--name", worker, "--format", "json"}, nil)
	if err != nil {
		return nil, err
	}
	secrets := []Secret{}
	if err := json.Unmarshal(out, &secrets); err != nil {
		return nil, fmt.Errorf("decode secrets: %w", err)
	}
	return secrets, nil
}

// PutSecret sends the value on stdin. Without deploy it goes into a new
// version that isn't deployed, so the live one keeps serving.
func PutSecret(ctx context.Context, worker, accountID, name, value string, deploy bool) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return putSecretWith(ctx, worker, accountID, name, value, deploy, runCLIInput)
}

func putSecretWith(ctx context.Context, worker, accountID, name, value string, deploy bool, run inputRunner) error {
	env, err := accountEnv(ctx, accountID, run)
	if err != nil {
		return err
	}
	_, err = run(ctx, env, secretArgs(deploy, "put", name, worker), []byte(value))
	return err
}

// DeleteSecret runs without a terminal, where wrangler takes its confirmation
// as given; the phone asks before calling.
func DeleteSecret(ctx context.Context, worker, accountID, name string, deploy bool) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return deleteSecretWith(ctx, worker, accountID, name, deploy, runCLIInput)
}

func deleteSecretWith(ctx context.Context, worker, accountID, name string, deploy bool, run inputRunner) error {
	env, err := accountEnv(ctx, accountID, run)
	if err != nil {
		return err
	}
	_, err = run(ctx, env, secretArgs(deploy, "delete", name, worker), nil)
	return err
}

// secretArgs deploys the change with `secret`, or only uploads a version
// with `versions secret`.
func secretArgs(deploy bool, verb, name, worker string) []string {
	args := []string{"secret", verb, name, "--name", worker}
	if !deploy {
		args = append([]string{"versions"}, args...)
	}
	return args
}

// accountEnv picks the Worker's account, or the login's only one.
func accountEnv(ctx context.Context, accountID string, run inputRunner) ([]string, error) {
	if accountID == "" {
		var err error
		accountID, err = onlyAccount(ctx, func(ctx context.Context, env, args []string) ([]byte, error) {
			return run(ctx, env, args, nil)
		})
		if err != nil {
			return nil, err
		}
	}
	return []string{"CLOUDFLARE_ACCOUNT_ID=" + accountID}, nil
}

package clitool

import (
	"cmp"
	"context"
	"os"
	"strings"

	"github.com/repogoapp/host/internal/par"
)

// awsProfiles are the AWS CLI's accounts: its profiles. One without stored
// keys signs in (again) with `aws login` here; one of stored keys has nothing
// to sign in, and deleting it deletes its keys. A project's agents get
// AWS_PROFILE.
type awsProfiles struct{}

func (awsProfiles) List(ctx context.Context, bin string) []CLIAccount {
	out, err := awsRun(ctx, bin, "configure", "list-profiles")
	if err != nil {
		return nil
	}
	ids := strings.Fields(out)
	// Two aws processes a profile, a few profiles at a time.
	return par.Map(ids, 4, func(id string) CLIAccount {
		a := CLIAccount{ID: id}
		// Any profile without stored keys signs in with `aws login`: one that
		// did already, and a bare one (a region and nothing else).
		_, keyErr := awsRun(ctx, bin, "configure", "get", "aws_access_key_id", "--profile", id)
		a.CanSignIn = keyErr != nil
		arn, err := awsRun(ctx, bin, "sts", "get-caller-identity", "--profile", id, "--query", "Arn", "--output", "text")
		switch {
		case err == nil && strings.HasPrefix(strings.TrimSpace(arn), "arn:"):
			a.SignedIn, a.Detail = true, awsWho(strings.TrimSpace(arn))
		case a.CanSignIn:
			a.Detail = "Signed out"
		default:
			a.Detail = "Its keys don't work"
		}
		return a
	})
}

func (awsProfiles) LoginArgs(id string) []string {
	return []string{"login", "--remote", "--profile", id}
}

// Prepare gives a new profile the default profile's region, so `aws login`
// doesn't stop to ask for one.
func (awsProfiles) Prepare(ctx context.Context, bin, id string) error {
	region, _ := awsRun(ctx, bin, "configure", "get", "region")
	region = strings.TrimSpace(region)
	if region == "" {
		region = "us-east-1"
	}
	_, err := awsRun(ctx, bin, "configure", "set", "region", region, "--profile", id)
	return err
}

func (awsProfiles) LoginEnv(string) []string { return nil }

// Default is the profile the CLI uses with none named: AWS_PROFILE as the
// host runs, else "default".
func (awsProfiles) Default(_ context.Context, _ string, accounts []CLIAccount) string {
	want := cmp.Or(os.Getenv("AWS_PROFILE"), "default")
	for _, a := range accounts {
		if a.ID == want {
			return a.ID
		}
	}
	return ""
}

func (awsProfiles) Env(id string) string { return "AWS_PROFILE=" + id }

func awsRun(ctx context.Context, bin string, args ...string) (string, error) {
	return runCLI(ctx, bin, nil, args...)
}

// awsWho is an identity's ARN as a person reads it: the account and who in
// it, "123456789012 · user/deploy" or "· Admin" for an assumed role.
func awsWho(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 {
		return arn
	}
	account, resource := parts[4], parts[5]
	if role, ok := strings.CutPrefix(resource, "assumed-role/"); ok {
		resource, _, _ = strings.Cut(role, "/")
	}
	return account + " · " + resource
}

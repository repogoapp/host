package clitool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The AWS CLI has no command to rename or remove a profile, so the host edits
// its files itself: only the one profile's section, after a timestamped copy
// of the file beside it.

// awsFiles are the AWS CLI's config and credentials files, as it finds them.
func awsFiles() (config, credentials string) {
	home, _ := os.UserHomeDir()
	config = os.Getenv("AWS_CONFIG_FILE")
	if config == "" {
		config = filepath.Join(home, ".aws", "config")
	}
	credentials = os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	if credentials == "" {
		credentials = filepath.Join(home, ".aws", "credentials")
	}
	return config, credentials
}

// configHeader is a profile's section in the config file; credentials name
// it bare.
func configHeader(id string) string {
	if id == "default" {
		return "[default]"
	}
	return "[profile " + id + "]"
}

// removeSection is text without the section header opens, through the line
// before the next section; found is whether it was there.
func removeSection(text, header string) (out string, found bool) {
	var keep []string
	in := false
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			in = trimmed == header
			found = found || in
		}
		if !in {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, ""), found
}

// renameSection is text with the header from renamed to to.
func renameSection(text, from, to string) (out string, found bool) {
	lines := strings.SplitAfter(text, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == from {
			lines[i] = strings.Replace(line, from, to, 1)
			found = true
		}
	}
	return strings.Join(lines, ""), found
}

// editAWSFile rewrites path through edit, keeping its permissions, after a
// copy beside it; a file that isn't there, or that edit leaves alone, is
// untouched.
func editAWSFile(path string, edit func(string) (string, bool)) error {
	before, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	after, changed := edit(string(before))
	if !changed {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	backup := fmt.Sprintf("%s.repogo-%d.bak", path, time.Now().Unix())
	if err := os.WriteFile(backup, before, 0o600); err != nil {
		return err
	}
	tmp := path + ".repogo.tmp"
	if err := os.WriteFile(tmp, []byte(after), info.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Remove signs the profile out and removes its sections: from the config
// file, and from the credentials file for one of stored keys.
func (awsProfiles) Remove(ctx context.Context, bin, id string) error {
	_, _ = awsRun(ctx, bin, "logout", "--profile", id) // nothing to sign out is fine
	config, credentials := awsFiles()
	if err := editAWSFile(config, func(text string) (string, bool) { return removeSection(text, configHeader(id)) }); err != nil {
		return err
	}
	return editAWSFile(credentials, func(text string) (string, bool) { return removeSection(text, "["+id+"]") })
}

// Rename renames the profile in the config file, and in the credentials file
// for one of stored keys.
func (awsProfiles) Rename(_ context.Context, _, from, to string) error {
	config, credentials := awsFiles()
	if err := editAWSFile(config, func(text string) (string, bool) {
		return renameSection(text, configHeader(from), configHeader(to))
	}); err != nil {
		return err
	}
	return editAWSFile(credentials, func(text string) (string, bool) {
		return renameSection(text, "["+from+"]", "["+to+"]")
	})
}

// Who is who the profile signs in as, and a name to suggest for it: the
// account's alias, else its last digits.
func (awsProfiles) Who(ctx context.Context, bin, id string) (detail, suggestion string) {
	arn, err := awsRun(ctx, bin, "sts", "get-caller-identity", "--profile", id, "--query", "Arn", "--output", "text")
	arn = strings.TrimSpace(arn)
	if err != nil || !strings.HasPrefix(arn, "arn:") {
		return "", ""
	}
	detail = awsWho(arn)
	alias, _ := awsRun(ctx, bin, "iam", "list-account-aliases", "--profile", id, "--query", "AccountAliases[0]", "--output", "text")
	if alias = strings.TrimSpace(alias); alias != "" && alias != "None" && accountName.MatchString(alias) {
		return detail, alias
	}
	account, _, _ := strings.Cut(detail, " ")
	return detail, "account-" + account[max(0, len(account)-4):]
}

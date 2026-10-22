package connect

import (
	"context"
	"path/filepath"

	"github.com/repogoapp/host/internal/git"
)

// An env key: one secret step, saved as a line of a dotenv file the host finds.

func (s *Service) envStatus(project string, key envKey, c *Connection) error {
	file, err := s.resolve(project, key.Scope)
	if err != nil {
		return err
	}
	c.Destination, c.Status = file.Destination, Missing
	if hasKey(file.Path, key.Name) {
		c.Status = Configured
	}
	return nil
}

func (s *Service) startEnv(ctx context.Context, f *flow, note, help string) error {
	file, err := s.resolve(f.project, f.target.Env.Scope)
	if err != nil {
		return err
	}
	name := f.target.Env.Name
	f.Destination, f.envPath = file.Destination, file.Path
	// Set already: the sheet opens on Connected, where it can be removed.
	if hasKey(file.Path, name) {
		f.end(Succeeded, "", s.d.Now())
		return nil
	}
	f.Step = &Step{ID: "secret", Kind: StepSecret, Label: name, Name: name, Note: note, HelpURL: help,
		Warning: s.committable(ctx, file.Path)}
	return nil
}

// disconnectEnv removes the key's lines from the file the flow saved to.
func (s *Service) disconnectEnv(f flow) error {
	if err := s.samePath(f); err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return removeKey(f.envPath, f.target.Env.Name)
}

// committable warns when git would pick the file up: it is tracked, or not
// ignored. Outside a repository, or one git can't read here, says nothing.
func (s *Service) committable(ctx context.Context, path string) string {
	root := git.Root(filepath.Dir(path))
	if root == "" {
		return ""
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return ""
	}
	ignored, err := s.d.Ignored(ctx, root, []string{rel})
	if err != nil || ignored[rel] {
		return ""
	}
	return filepath.Base(path) + " isn't ignored by git, so the key could be committed."
}

func (s *Service) submitEnv(f flow, value string) error {
	line, err := envLine(f.target.Env.Name, value)
	if err != nil {
		return err
	}
	if err := s.samePath(f); err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return writeKey(f.envPath, f.target.Env.Name, line)
}

// samePath refuses a file that now resolves somewhere other than where the
// sheet said (a folder swapped for a link), so a write or removal goes there
// or nowhere.
func (s *Service) samePath(f flow) error {
	file, err := s.resolve(f.project, f.target.Env.Scope)
	if err != nil {
		return err
	}
	if file.Path != f.envPath {
		return errRefused.Errorf("where this is saved has changed; start again")
	}
	return nil
}

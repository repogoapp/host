package clitool

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// A CLI's own sign-out only deletes a file or keychain item, so a long wait
// means it is stuck on a prompt.
const logoutTimeout = 30 * time.Second

// Logout runs the CLI's own sign-out and re-probes it. A failed sign-out is
// reported in the result; only an unknown kind is an error.
func (in *Inventory) Logout(ctx context.Context, kind string) (UpdateResult, error) {
	res := UpdateResult{Kind: kind}

	t, ok := in.Lookup(kind)
	if !ok || t.Spec.LogoutArgs == nil {
		return res, fmt.Errorf("%w %q", ErrUnknownTool, kind)
	}
	current, _ := in.find(ctx, kind)
	if !current.Installed {
		res.Error = current.Name + " is not installed"
		return res, nil
	}
	if !current.Authed {
		res.OK = true
		return res, nil
	}
	defer in.changed()
	ctx, cancel := context.WithTimeout(ctx, logoutTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, current.path, t.Spec.LogoutArgs...)
	// The sign-in's environment keeps the CLI off prompts and browsers here too.
	cmd.Env = append(cmd.Environ(), t.Spec.LoginEnv...)
	cmd.Dir = os.TempDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		res.Error = err.Error()
		in.ranFailed(kind, append([]string{current.Command}, t.Spec.LogoutArgs...), out, err)
		return res, nil
	}

	after, _ := in.find(ctx, kind)
	if after.Authed {
		res.Error = current.Name + " signed out but still has credentials (" + after.authSource + ")"
		return res, nil
	}
	res.OK = true
	return res, nil
}

// Uninstall removes a CLI the way it was installed. It leaves the CLI's
// config directory alone: session sync reads the user's history from there.
func (in *Inventory) Uninstall(ctx context.Context, kind string) (UpdateResult, error) {
	res := UpdateResult{Kind: kind}

	current, known := in.find(ctx, kind)
	if !known {
		return res, fmt.Errorf("%w %q", ErrUnknownTool, kind)
	}
	if !current.Installed {
		res.OK = true
		return res, nil
	}
	t, _ := in.Lookup(kind)
	if !canUninstall(current.manager, t.Spec) {
		res.Error = "nothing here knows how to remove a " + current.manager + " install of " + current.Name
		return res, nil
	}
	defer in.changed()

	if current.manager == "native" {
		home, _ := os.UserHomeDir()
		target := nativeTarget(current.path, t.Spec, home)
		if target == "" {
			res.Error = current.path + " is not where " + current.Name + "'s installer puts it"
			return res, nil
		}
		in.updateMu.Lock()
		err := os.Remove(target)
		in.updateMu.Unlock()
		if err != nil {
			res.Error = err.Error()
			return res, nil
		}
	} else {
		args := uninstallCommandOf(current.manager, t.Spec)
		if !in.runLocked(ctx, &res, args[0], args[1:]...) {
			return res, nil
		}
	}

	if after, _ := in.find(ctx, kind); after.Installed {
		res.Error = "removed " + current.path + " but another " + current.Command + " is at " + after.path
		return res, nil
	}
	res.OK = true
	return res, nil
}

// canUninstall is whether Uninstall knows how to remove an install made by
// manager. Unknown is never guessed, for the reason updateCommandOf gives.
func canUninstall(manager string, s Spec) bool {
	return manager == "native" || uninstallCommandOf(manager, s) != nil
}

func uninstallCommandOf(manager string, s Spec) []string {
	switch {
	case manager == "npm" && s.Pkg != "":
		return []string{"npm", "uninstall", "-g", s.Pkg}
	case manager == "bun" && s.Pkg != "":
		return []string{"bun", "remove", "-g", s.Pkg}
	case manager == "pnpm" && s.Pkg != "":
		return []string{"pnpm", "remove", "-g", s.Pkg}
	case manager == "homebrew" && s.Formula != "":
		return []string{"brew", "uninstall", s.Formula}
	}
	return nil
}

// nativeTarget is the file a native uninstall removes: path, or what it links
// to, when that is one of the tool's own install locations; else "". So a
// native uninstall never removes a file the tool's installer did not put there.
func nativeTarget(path string, s Spec, home string) string {
	candidates := []string{path}
	if real, err := filepath.EvalSymlinks(path); err == nil && real != path {
		candidates = append(candidates, real)
	}
	for _, c := range candidates {
		for _, rel := range s.NativePaths {
			if home != "" && c == filepath.Join(home, rel) {
				return c
			}
		}
	}
	return ""
}

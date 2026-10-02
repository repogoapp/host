// Package service manages the user's background host.
package service

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/repogo/host/internal/apphome"
	"github.com/repogo/host/internal/syscmd"
)

const label = "app.repogo.host"

type Service struct {
	path string
	log  string
	home string
}

func New() (*Service, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	s := &Service{home: home}
	switch runtime.GOOS {
	case "darwin":
		s.path = filepath.Join(home, "Library/LaunchAgents", label+".plist")
		s.log = filepath.Join(home, "Library/Logs/repogo/host.log")
	case "linux":
		config, err := os.UserConfigDir()
		if err != nil {
			return nil, err
		}
		s.path = filepath.Join(config, "systemd/user", label+".service")
		if s.log, err = apphome.Path("host.log"); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("repogo supports macOS and Linux")
	}
	return s, nil
}

func (s *Service) Installed() (bool, error) {
	_, err := os.Stat(s.path)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func (s *Service) Install(ctx context.Context, binary string) error {
	// The invoking shell's PATH includes the user's Node and agent installations.
	body := s.unit(runtime.GOOS, binary, os.Getenv("PATH"))
	for _, dir := range []string{filepath.Dir(s.path), filepath.Dir(s.log)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	if err := os.WriteFile(s.path, []byte(body), 0o600); err != nil {
		return err
	}
	if runtime.GOOS == "linux" {
		return command(ctx, "systemctl", "--user", "daemon-reload")
	}
	return nil
}

// unit is the launchd plist (darwin) or systemd unit that runs binary with path.
func (s *Service) unit(goos, binary, path string) string {
	if goos == "darwin" {
		escape := func(v string) string { var b bytes.Buffer; _ = xml.EscapeText(&b, []byte(v)); return b.String() }
		return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>serve</string></array>
<key>EnvironmentVariables</key><dict><key>PATH</key><string>%s</string></dict>
<key>WorkingDirectory</key><string>%s</string>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
<key>ThrottleInterval</key><integer>5</integer>
<key>ExitTimeOut</key><integer>30</integer>
<key>ProcessType</key><string>Interactive</string>
<key>StandardOutPath</key><string>%s</string>
<key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, label, escape(binary), escape(path), escape(s.home), escape(s.log), escape(s.log))
	}
	// systemd expands percent specifiers and ExecStart expands dollar variables.
	quote := func(v string) string { return strconv.Quote(strings.ReplaceAll(v, "%", "%%")) }
	return fmt.Sprintf(`[Unit]
Description=RepoGo host
[Service]
ExecStart=%s serve
WorkingDirectory=%s
Environment=%s
Restart=always
RestartSec=5
TimeoutStopSec=30
StandardOutput=append:%s
StandardError=append:%s
[Install]
WantedBy=default.target
`, quote(strings.ReplaceAll(binary, "$", "$$")), quote(s.home), quote("PATH="+path), strings.ReplaceAll(s.log, "%", "%%"), strings.ReplaceAll(s.log, "%", "%%"))
}

func (s *Service) loaded(ctx context.Context) bool {
	if runtime.GOOS == "darwin" {
		return command(ctx, "launchctl", "print", s.target()) == nil
	}
	return command(ctx, "systemctl", "--user", "is-active", "--quiet", label) == nil
}

func (s *Service) target() string { return fmt.Sprintf("gui/%d/%s", os.Getuid(), label) }

func (s *Service) Start(ctx context.Context) error {
	if s.loaded(ctx) {
		return nil
	}
	if runtime.GOOS == "darwin" {
		if err := command(ctx, "launchctl", "enable", s.target()); err != nil {
			return err
		}
		return command(ctx, "launchctl", "bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), s.path)
	}
	return command(ctx, "systemctl", "--user", "enable", "--now", label)
}

func (s *Service) Stop(ctx context.Context) error {
	if runtime.GOOS == "darwin" {
		if !s.loaded(ctx) {
			return nil
		}
		return command(ctx, "launchctl", "bootout", s.target())
	}
	installed, err := s.Installed()
	if err != nil || !installed {
		return err
	}
	return command(ctx, "systemctl", "--user", "stop", label)
}

func (s *Service) Restart(ctx context.Context) error {
	if runtime.GOOS == "darwin" {
		if !s.loaded(ctx) {
			return s.Start(ctx)
		}
		return command(ctx, "launchctl", "kickstart", "-k", s.target())
	}
	return command(ctx, "systemctl", "--user", "restart", label)
}

func (s *Service) Uninstall(ctx context.Context) error {
	installed, err := s.Installed()
	if err != nil || !installed {
		return err
	}
	if err := s.Stop(ctx); err != nil {
		return err
	}
	if runtime.GOOS == "linux" {
		if err := command(ctx, "systemctl", "--user", "disable", label); err != nil {
			return err
		}
	}
	if err := os.Remove(s.path); err != nil {
		return err
	}
	if runtime.GOOS == "linux" {
		return command(ctx, "systemctl", "--user", "daemon-reload")
	}
	return nil
}

func (s *Service) Logs() error {
	cmd := exec.Command("tail", "-n", "50", "-F", s.log)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func command(ctx context.Context, name string, args ...string) error {
	_, err := syscmd.Output(ctx, 30*time.Second, name, args...)
	return err
}

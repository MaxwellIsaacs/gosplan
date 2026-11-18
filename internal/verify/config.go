package verify

import (
	"fmt"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/MaxwellIsaacs/gosplan/internal/gitx"
)

// ConfigPath is the project config file, read from the base commit.
const ConfigPath = "nightrunner.toml"

// Config is the subset of nightrunner.toml the verifier uses.
// Key names match the existing bash orchestrator.
type Config struct {
	Verify struct {
		Test     string   `toml:"test"`
		Lint     string   `toml:"lint"`
		Build    string   `toml:"build"`
		Custom   []string `toml:"custom"`
		Timeouts struct {
			TestSeconds   int `toml:"test_seconds"`
			LintSeconds   int `toml:"lint_seconds"`
			BuildSeconds  int `toml:"build_seconds"`
			CustomSeconds int `toml:"custom_seconds"`
		} `toml:"timeouts"`
		Protected struct {
			Paths []string `toml:"paths"`
		} `toml:"protected"`
		Locked struct {
			Paths []string `toml:"paths"`
		} `toml:"locked"`
	} `toml:"verify"`
}

// Command is one runnable verification step, resolved from the config.
type Command struct {
	Name    string // "test", "lint", "build", or "custom[i]"
	Shell   string
	Timeout time.Duration
}

// DefaultProtected is always in force. `**/testdata/**` (not `testdata/**`)
// because Go keeps testdata per package. Project globs are added to it.
var DefaultProtected = []string{"**/*_test.go", "tests/**", "**/testdata/**"}

// DefaultLocked is always in force: these files may not change in a task
// branch, whatever the scope says. They configure the verifier or the agents.
// Project globs are added to it, never substituted for it.
var DefaultLocked = []string{
	ConfigPath, ".gitattributes", ".mcp.json", ".claude/**", "CLAUDE.md",
}

// LoadConfig reads nightrunner.toml from the base commit. It never reads the
// worker's working tree: that is the trust rule in 01-verify-contract.md.
func LoadConfig(r gitx.Repo, base string) (*Config, error) {
	src, ok, err := r.ShowFile(base, ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("verify: read %s at base: %w", ConfigPath, err)
	}
	if !ok {
		return nil, fmt.Errorf("verify: %s not present at base %s", ConfigPath, base)
	}
	var c Config
	if _, err := toml.Decode(src, &c); err != nil {
		return nil, fmt.Errorf("verify: parse %s: %w", ConfigPath, err)
	}
	return &c, nil
}

// Protected returns the protected-path globs: the defaults plus the project's.
func (c *Config) Protected() []string {
	return append(append([]string{}, DefaultProtected...), c.Verify.Protected.Paths...)
}

// LockedPaths returns the locked-path globs: the defaults plus the project's.
func (c *Config) LockedPaths() []string {
	return append(append([]string{}, DefaultLocked...), c.Verify.Locked.Paths...)
}

// Commands returns the configured verification commands in gate order:
// test, lint, build, then custom[] in list order. Empty commands are skipped.
func (c *Config) Commands() []Command {
	t := c.Verify.Timeouts
	secs := func(v, def int) time.Duration {
		if v <= 0 {
			v = def
		}
		return time.Duration(v) * time.Second
	}
	var out []Command
	add := func(name, shell string, timeout time.Duration) {
		if shell != "" {
			out = append(out, Command{Name: name, Shell: shell, Timeout: timeout})
		}
	}
	add("test", c.Verify.Test, secs(t.TestSeconds, 600))
	add("lint", c.Verify.Lint, secs(t.LintSeconds, 120))
	add("build", c.Verify.Build, secs(t.BuildSeconds, 600))
	for i, s := range c.Verify.Custom {
		add(fmt.Sprintf("custom[%d]", i), s, secs(t.CustomSeconds, 300))
	}
	return out
}

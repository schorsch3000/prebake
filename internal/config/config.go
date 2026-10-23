// Package config defines the YAML schema of the config repo (prebake.yaml and
// containers/<name>/container.yaml) and loads it strictly: unknown keys are errors.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	GlobalFile    = "prebake.yaml"
	ContainerFile = "container.yaml"
	ContainersDir = "containers"
)

type Global struct {
	// RootFiles are extra files and directories (globs) that may sit at the repo root next to
	// prebake.yaml and containers/. Anything else there is a stray file.
	RootFiles  []string `yaml:"root_files"`
	Slots      []Slot   `yaml:"slots"`
	Defaults   Defaults `yaml:"defaults"`
	RetryDelay Duration `yaml:"retry_delay"`
	Keep       int      `yaml:"keep"`
	Registry   Registry `yaml:"registry"`
	Notify     Notify   `yaml:"notify"`
}

type Defaults struct {
	RebuildInterval Duration `yaml:"rebuild_interval"`
	RefreshInterval Duration `yaml:"refresh_interval"`
}

type Registry struct {
	GCSlotOnly *bool `yaml:"gc_slot_only"`
	// PruneRemoved removes the images of containers and flavors that are no longer in the
	// config, PruneRemovedAfter later (a grace period, so that a commit that removes something
	// by mistake can be reverted). Defaults: true and 7d.
	PruneRemoved      *bool    `yaml:"prune_removed"`
	PruneRemovedAfter Duration `yaml:"prune_removed_after"`
}

type Notify struct {
	Pushover Pushover `yaml:"pushover"`
}

// Pushover priorities, -2 (silent) to 1 (high). Priority 2 (emergency) needs
// retry/expire handling and is not supported. Nil means "use the default".
type Pushover struct {
	PriorityFailure  *int `yaml:"priority_failure"`
	PriorityRecovery *int `yaml:"priority_recovery"`
}

type Container struct {
	RefreshCmd      string            `yaml:"refresh_cmd"`
	RefreshInterval Duration          `yaml:"refresh_interval"`
	RebuildInterval Duration          `yaml:"rebuild_interval"`
	Keep            int               `yaml:"keep"`
	DependsOn       []string          `yaml:"depends_on"`
	ExtraFiles      []string          `yaml:"extra_files"`
	Flavors         map[string]Flavor `yaml:"flavors"`
	Tests           []string          `yaml:"tests"`
}

type Flavor struct {
	Args map[string]string `yaml:"args"`
}

// DefaultFlavor is used when a container declares no flavors.
const DefaultFlavor = "default"

var nameRe = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

// ValidName reports whether s is usable as image repository name or flavor name.
func ValidName(s string) bool { return len(s) <= 128 && nameRe.MatchString(s) }

// ApplyDefaults fills in values the user may omit.
func (g *Global) ApplyDefaults() {
	if len(g.Slots) == 0 {
		g.Slots = []Slot{{Start: 2 * time.Hour, End: 6 * time.Hour}}
	}
	if g.Defaults.RebuildInterval == 0 {
		g.Defaults.RebuildInterval = Duration(7 * 24 * time.Hour)
	}
	if g.Defaults.RefreshInterval == 0 {
		g.Defaults.RefreshInterval = Duration(24 * time.Hour)
	}
	if g.RetryDelay == 0 {
		g.RetryDelay = Duration(15 * time.Minute)
	}
	if g.Keep == 0 {
		g.Keep = 5
	}
	if g.Notify.Pushover.PriorityFailure == nil {
		v := 1
		g.Notify.Pushover.PriorityFailure = &v
	}
	if g.Notify.Pushover.PriorityRecovery == nil {
		v := -1
		g.Notify.Pushover.PriorityRecovery = &v
	}
	if g.Registry.PruneRemoved == nil {
		t := true
		g.Registry.PruneRemoved = &t
	}
	if g.Registry.PruneRemovedAfter == 0 {
		g.Registry.PruneRemovedAfter = Duration(7 * 24 * time.Hour)
	}
	if g.Registry.GCSlotOnly == nil {
		t := true
		g.Registry.GCSlotOnly = &t
	}
}

func (g *Global) check() error {
	if g.Keep < 0 {
		return errors.New("keep must be >= 1")
	}
	return nil
}

func (c *Container) check() error {
	if c.Keep < 0 {
		return errors.New("keep must be >= 1")
	}
	for name := range c.Flavors {
		if !ValidName(name) {
			return fmt.Errorf("invalid flavor name %q", name)
		}
	}
	return nil
}

// FlavorNames returns the flavors to build; DefaultFlavor if none are declared.
func (c *Container) FlavorNames() []string {
	if len(c.Flavors) == 0 {
		return []string{DefaultFlavor}
	}
	names := make([]string, 0, len(c.Flavors))
	for n := range c.Flavors {
		names = append(names, n)
	}
	sortStrings(names)
	return names
}

// Decode strictly decodes YAML from r into v. Errors carry line numbers.
func Decode(r io.Reader, v any) error {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // empty file: all defaults
		}
		return err
	}
	return nil
}

func LoadGlobal(path string) (*Global, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseGlobal(path, b)
}

func ParseGlobal(name string, b []byte) (*Global, error) {
	var g Global
	if err := Decode(bytes.NewReader(b), &g); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if err := g.check(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	g.ApplyDefaults()
	return &g, nil
}

func LoadContainer(path string) (*Container, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseContainer(path, b)
}

func ParseContainer(name string, b []byte) (*Container, error) {
	var c Container
	if err := Decode(bytes.NewReader(b), &c); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if err := c.check(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return &c, nil
}

// Effective resolves per-container overrides against the global config.
type Effective struct {
	RebuildInterval Duration
	RefreshInterval Duration
	Keep            int
}

func (c *Container) Effective(g *Global) Effective {
	e := Effective{
		RebuildInterval: g.Defaults.RebuildInterval,
		RefreshInterval: g.Defaults.RefreshInterval,
		Keep:            g.Keep,
	}
	if c.RebuildInterval != 0 {
		e.RebuildInterval = c.RebuildInterval
	}
	if c.RefreshInterval != 0 {
		e.RefreshInterval = c.RefreshInterval
	}
	if c.Keep != 0 {
		e.Keep = c.Keep
	}
	return e
}

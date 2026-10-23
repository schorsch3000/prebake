// Package validate checks a prebake config repo. It is pure and offline: it
// only reads files through an fs.FS and never touches Docker or the network.
package validate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/schorsch3000/prebake/internal/config"
)

type Severity int

const (
	Error Severity = iota
	Warning
)

func (s Severity) String() string {
	if s == Warning {
		return "warning"
	}
	return "error"
}

// Issue is one finding. Line is 0 when unknown.
type Issue struct {
	Severity Severity
	File     string // slash-separated path relative to the repo root
	Line     int
	Msg      string
}

func (i Issue) String() string {
	loc := i.File
	if i.Line > 0 {
		loc = fmt.Sprintf("%s:%d", i.File, i.Line)
	}
	if i.Severity == Warning {
		return fmt.Sprintf("%s: warning: %s", loc, i.Msg)
	}
	return fmt.Sprintf("%s: %s", loc, i.Msg)
}

// Result of a validation run.
type Result struct {
	Issues []Issue
	Global *config.Global // nil if prebake.yaml was unusable
}

func (r *Result) Errors() int   { return r.count(Error) }
func (r *Result) Warnings() int { return r.count(Warning) }
func (r *Result) count(s Severity) int {
	n := 0
	for _, i := range r.Issues {
		if i.Severity == s {
			n++
		}
	}
	return n
}

func (r *Result) add(sev Severity, file string, line int, format string, a ...any) {
	r.Issues = append(r.Issues, Issue{sev, file, line, fmt.Sprintf(format, a...)})
}

// Dir validates the repo rooted at dir.
func Dir(dir string) *Result { return FS(os.DirFS(dir)) }

// FS validates the repo whose root is fsys.
func FS(fsys fs.FS) *Result {
	r := &Result{}
	r.global(fsys)
	r.containers(fsys)
	sort.SliceStable(r.Issues, func(i, j int) bool {
		a, b := r.Issues[i], r.Issues[j]
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	return r
}

// errLine extracts "line N" from yaml/config errors, 0 if absent.
func errLine(err error) int {
	s := err.Error()
	i := strings.Index(s, "line ")
	if i < 0 {
		return 0
	}
	var n int
	if _, e := fmt.Sscanf(s[i:], "line %d", &n); e != nil {
		return 0
	}
	return n
}

// errMsg strips the "<file>: " prefix added by the config loader.
func errMsg(file string, err error) string {
	return strings.TrimPrefix(err.Error(), file+": ")
}

// keyLine finds the line of a top-level key in YAML data (0 if not found).
func keyLine(data []byte, key string) int {
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) == 0 {
		return 0
	}
	m := doc.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i].Line
		}
	}
	return 0
}

func (r *Result) global(fsys fs.FS) {
	data, err := fs.ReadFile(fsys, config.GlobalFile)
	if errors.Is(err, fs.ErrNotExist) {
		r.add(Error, config.GlobalFile, 0, "missing global config")
		return
	} else if err != nil {
		r.add(Error, config.GlobalFile, 0, "%v", err)
		return
	}
	g, err := config.ParseGlobal(config.GlobalFile, data)
	if err != nil {
		r.add(Error, config.GlobalFile, errLine(err), "%s", errMsg(config.GlobalFile, err))
		return
	}
	r.Global = g
	for name, p := range map[string]*int{"priority_failure": g.Notify.Pushover.PriorityFailure, "priority_recovery": g.Notify.Pushover.PriorityRecovery} {
		if p != nil && (*p < -2 || *p > 1) {
			r.add(Error, config.GlobalFile, 0, "notify.pushover.%s %d out of range (-2..1; emergency priority 2 is not supported)", name, *p)
		}
	}
	for i := range g.Slots {
		for j := i + 1; j < len(g.Slots); j++ {
			if slotsOverlap(g.Slots[i], g.Slots[j]) {
				r.add(Error, config.GlobalFile, keyLine(data, "slots"), "slots %s and %s overlap", g.Slots[i], g.Slots[j])
			}
		}
	}
}

func slotsOverlap(a, b config.Slot) bool {
	const day = 24 * 60 * 60 * 1e9
	// split midnight-crossing slots into plain intervals on [0, day]
	split := func(s config.Slot) [][2]int64 {
		if s.End > s.Start {
			return [][2]int64{{int64(s.Start), int64(s.End)}}
		}
		return [][2]int64{{int64(s.Start), day}, {0, int64(s.End)}}
	}
	for _, x := range split(a) {
		for _, y := range split(b) {
			if x[0] < y[1] && y[0] < x[1] {
				return true
			}
		}
	}
	return false
}

type container struct {
	name string
	cfg  *config.Container
}

func (r *Result) containers(fsys fs.FS) {
	entries, err := fs.ReadDir(fsys, config.ContainersDir)
	if errors.Is(err, fs.ErrNotExist) {
		r.add(Error, config.ContainersDir, 0, "missing %s/ directory", config.ContainersDir)
		return
	} else if err != nil {
		r.add(Error, config.ContainersDir, 0, "%v", err)
		return
	}
	var cs []container
	for _, e := range entries {
		rel := path.Join(config.ContainersDir, e.Name())
		if !e.IsDir() {
			r.add(Error, rel, 0, "stray file directly under %s/ (only container directories allowed)", config.ContainersDir)
			continue
		}
		if c := r.container(fsys, e.Name()); c != nil {
			cs = append(cs, *c)
		}
	}
	r.dependencies(cs)
}

func (r *Result) container(fsys fs.FS, name string) *container {
	dir := path.Join(config.ContainersDir, name)
	if !config.ValidName(name) {
		r.add(Error, dir, 0, "invalid container name %q (lower-case letters, digits, '.', '_', '-')", name)
	}
	cfgPath := path.Join(dir, config.ContainerFile)
	dfPath := path.Join(dir, "Dockerfile")

	var cfg *config.Container
	data, err := fs.ReadFile(fsys, cfgPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.add(Error, cfgPath, 0, "missing %s", config.ContainerFile)
	case err != nil:
		r.add(Error, cfgPath, 0, "%v", err)
	default:
		cfg, err = config.ParseContainer(cfgPath, data)
		if err != nil {
			r.add(Error, cfgPath, errLine(err), "%s", errMsg(cfgPath, err))
		}
	}
	if cfg != nil {
		if r.Global != nil && cfg.Keep > r.Global.Keep {
			r.add(Error, cfgPath, keyLine(data, "keep"), "keep %d exceeds global keep %d", cfg.Keep, r.Global.Keep)
		}
		if len(cfg.Tests) == 0 {
			r.add(Warning, cfgPath, 0, "no tests configured; images are promoted without a smoke test")
		}
		for _, t := range cfg.Tests {
			if strings.TrimSpace(t) == "" {
				r.add(Error, cfgPath, keyLine(data, "tests"), "empty test command")
			}
		}
		for _, p := range cfg.ExtraFiles {
			if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
				r.add(Error, cfgPath, keyLine(data, "extra_files"), "invalid extra_files pattern %q", p)
			}
		}
	}

	var df []instruction
	dfData, err := fs.ReadFile(fsys, dfPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.add(Error, dfPath, 0, "missing Dockerfile")
	case err != nil:
		r.add(Error, dfPath, 0, "%v", err)
	default:
		df = parseDockerfile(string(dfData))
		if len(df) == 0 {
			r.add(Error, dfPath, 0, "Dockerfile has no instructions")
		}
	}

	referenced := r.checkCopies(fsys, dir, dfPath, df)
	if cfg != nil && len(cfg.DependsOn) > 0 && df != nil && !declaresArg(df, "PARENT") {
		r.add(Warning, dfPath, 0, "depends_on is set but the Dockerfile has no 'ARG PARENT' to receive the parent image")
	}
	var extra []string
	if cfg != nil {
		extra = cfg.ExtraFiles
	}
	r.strayFiles(fsys, dir, referenced, extra)

	if cfg == nil {
		return nil
	}
	return &container{name: name, cfg: cfg}
}

func declaresArg(df []instruction, name string) bool {
	for _, in := range df {
		if in.Cmd == "ARG" {
			n, _, _ := strings.Cut(in.Args, "=")
			if strings.TrimSpace(n) == name {
				return true
			}
		}
	}
	return false
}

// checkCopies verifies COPY/ADD sources and returns the referenced paths
// (relative to the container dir, cleaned) that stray-file detection must allow.
func (r *Result) checkCopies(fsys fs.FS, dir, dfPath string, df []instruction) []string {
	var refs []string
	for _, in := range df {
		if in.Cmd != "COPY" && in.Cmd != "ADD" {
			continue
		}
		srcs, fromStage := copySources(in.Args)
		if fromStage {
			continue
		}
		for _, s := range srcs {
			if strings.HasPrefix(s, "<<") || isURL(s) {
				continue
			}
			if strings.ContainsAny(s, "$") {
				continue // build-arg dependent, cannot check statically
			}
			clean := path.Clean(strings.TrimPrefix(s, "./"))
			if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
				r.add(Error, dfPath, in.Line, "%s source %q escapes the container directory", in.Cmd, s)
				continue
			}
			matches, err := fs.Glob(fsys, path.Join(dir, clean))
			if err != nil {
				r.add(Error, dfPath, in.Line, "%s source %q: bad pattern: %v", in.Cmd, s, err)
				continue
			}
			if len(matches) == 0 {
				r.add(Error, dfPath, in.Line, "%s source %q does not exist", in.Cmd, s)
				continue
			}
			refs = append(refs, clean)
		}
	}
	return refs
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "git@")
}

// strayFiles reports files in the container dir that nothing refers to.
func (r *Result) strayFiles(fsys fs.FS, dir string, referenced, extra []string) {
	// a path is allowed if it, or any ancestor directory of it, matches a pattern
	matchesAny := func(patterns []string, rel string) bool {
		segs := strings.Split(rel, "/")
		for i := 1; i <= len(segs); i++ {
			prefix := strings.Join(segs[:i], "/")
			for _, p := range patterns {
				if matchGlob(strings.TrimSuffix(p, "/"), prefix) {
					return true
				}
			}
		}
		return false
	}
	allowed := func(rel string) bool {
		switch rel {
		case "Dockerfile", config.ContainerFile, ".dockerignore":
			return true
		}
		return matchesAny(referenced, rel) || matchesAny(extra, rel)
	}
	_ = fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(p, dir+"/")
		if !allowed(rel) {
			r.add(Error, p, 0, "stray file: not referenced by the Dockerfile and not allowed by extra_files")
		}
		return nil
	})
}

func hasDefaultFlavor(c *config.Container) bool {
	if len(c.Flavors) == 0 {
		return true
	}
	_, ok := c.Flavors[config.DefaultFlavor]
	return ok
}

// dependencies checks depends_on targets and cycles.
func (r *Result) dependencies(cs []container) {
	byName := map[string]*container{}
	for i := range cs {
		byName[cs[i].name] = &cs[i]
	}
	for _, c := range cs {
		file := path.Join(config.ContainersDir, c.name, config.ContainerFile)
		for _, d := range c.cfg.DependsOn {
			switch {
			case d == c.name:
				r.add(Error, file, 0, "depends_on refers to itself")
			case byName[d] == nil:
				r.add(Error, file, 0, "depends_on %q: no such container", d)
			case !hasDefaultFlavor(byName[d].cfg):
				// children build FROM <registry>/<parent>:latest, which is the default flavor
				r.add(Error, file, 0, "depends_on %q: it declares flavors but no %q flavor, so %s:latest is never built", d, config.DefaultFlavor, d)
			}
		}
	}
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var stack []string
	reported := map[string]bool{}
	var visit func(n string)
	visit = func(n string) {
		color[n] = grey
		stack = append(stack, n)
		for _, d := range byName[n].cfg.DependsOn {
			if byName[d] == nil || d == n {
				continue
			}
			if color[d] == grey {
				i := len(stack) - 1
				for stack[i] != d {
					i--
				}
				cycle := append(append([]string{}, stack[i:]...), d)
				key := strings.Join(cycle, ">")
				if !reported[key] {
					reported[key] = true
					r.add(Error, path.Join(config.ContainersDir, d, config.ContainerFile), 0,
						"dependency cycle: %s", strings.Join(cycle, " -> "))
				}
			} else if color[d] == white {
				visit(d)
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if color[n] == white {
			visit(n)
		}
	}
}

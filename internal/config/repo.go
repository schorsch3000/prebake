package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Repo is a loaded config repo checkout.
type Repo struct {
	Dir        string
	Global     *Global
	Containers map[string]*Container
}

// LoadRepo loads prebake.yaml and every container config under dir. It does
// not validate cross-file rules; run validate.Dir first.
func LoadRepo(dir string) (*Repo, error) {
	g, err := LoadGlobal(filepath.Join(dir, GlobalFile))
	if err != nil {
		return nil, err
	}
	r := &Repo{Dir: dir, Global: g, Containers: map[string]*Container{}}
	entries, err := os.ReadDir(filepath.Join(dir, ContainersDir))
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		c, err := LoadContainer(filepath.Join(dir, ContainersDir, e.Name(), ContainerFile))
		if err != nil {
			return nil, fmt.Errorf("container %s: %w", e.Name(), err)
		}
		r.Containers[e.Name()] = c
	}
	return r, nil
}

// ContainerDir is the build context of a container.
func (r *Repo) ContainerDir(name string) string {
	return filepath.Join(r.Dir, ContainersDir, name)
}

// Names returns the container names, sorted.
func (r *Repo) Names() []string {
	names := make([]string, 0, len(r.Containers))
	for n := range r.Containers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Parents returns the depends_on graph for the job queue.
func (r *Repo) Parents() map[string][]string {
	p := map[string][]string{}
	for n, c := range r.Containers {
		if len(c.DependsOn) > 0 {
			p[n] = c.DependsOn
		}
	}
	return p
}

package agents

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

// Registry is an immutable, in-memory set of agent definitions. It satisfies
// core.AgentRegistry. Being immutable after construction, it is safe for
// concurrent use by every worker without locking.
type Registry struct {
	byName map[string]*core.AgentDefinition
}

// NewRegistry builds a registry from already-validated definitions.
func NewRegistry(defs ...*core.AgentDefinition) (*Registry, error) {
	r := &Registry{byName: make(map[string]*core.AgentDefinition, len(defs))}
	for _, d := range defs {
		if _, dup := r.byName[d.Name]; dup {
			return nil, fmt.Errorf("duplicate agent %q: %w", d.Name, core.ErrAlreadyExists)
		}
		r.byName[d.Name] = d
	}
	return r, nil
}

// Get returns the named agent or an error wrapping core.ErrNotFound.
func (r *Registry) Get(name string) (*core.AgentDefinition, error) {
	d, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("agent %q: %w (known agents: %s)",
			name, core.ErrNotFound, strings.Join(r.names(), ", "))
	}
	return d, nil
}

// List returns every agent, sorted by name.
func (r *Registry) List() []*core.AgentDefinition {
	out := make([]*core.AgentDefinition, 0, len(r.byName))
	for _, d := range r.byName {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) names() []string {
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// LoadFS reads every *.yaml and *.yml file in dir from fsys.
func LoadFS(fsys fs.FS, dir string) (*Registry, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read agents directory %s: %w", dir, err)
	}

	var defs []*core.AgentDefinition
	for _, e := range entries {
		if e.IsDir() || !isYAML(e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		def, err := parse(data, baseName(e.Name()))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		defs = append(defs, def)
	}
	if len(defs) == 0 {
		return nil, fmt.Errorf("no agent definitions found in %s: %w", dir, core.ErrNotFound)
	}
	return NewRegistry(defs...)
}

// LoadDir reads agent definitions from a directory on the real filesystem.
func LoadDir(dir string) (*Registry, error) {
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("agents directory %s does not exist: %w", dir, core.ErrNotFound)
		}
		return nil, fmt.Errorf("stat agents directory: %w", err)
	}
	return LoadFS(os.DirFS(dir), ".")
}

// parse decodes a single agent YAML document.
func parse(data []byte, defaultName string) (*core.AgentDefinition, error) {
	var s spec
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // reject typos instead of silently ignoring them
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parse agent yaml: %w", err)
	}
	return s.toDomain(defaultName)
}

func isYAML(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".yaml" || ext == ".yml"
}

func baseName(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name))
}

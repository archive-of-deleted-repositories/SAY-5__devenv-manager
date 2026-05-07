package imagebuild

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// AllowedTemplates is the canonical set of templates this build endpoint
// will resolve. Adding a template requires committing both:
//
//   - infra/templates/<name>/Dockerfile
//   - the entry below
//
// This duplication is the security boundary: arbitrary Dockerfiles
// cannot reach the engine even if the disk-backed registry is somehow
// pointed at attacker-controlled content.
var AllowedTemplates = []string{
	"node22-typescript",
	"python312-poetry",
	"rust-stable",
}

// DiskRegistry resolves templates by reading
// `<root>/<name>/Dockerfile`. The allowlist is the source of truth;
// the disk read is gated by the allowlist check.
type DiskRegistry struct {
	root string
}

// NewDiskRegistry constructs a DiskRegistry rooted at the given path.
func NewDiskRegistry(root string) *DiskRegistry { return &DiskRegistry{root: root} }

// List returns the canonical template names in lexical order.
func (d *DiskRegistry) List() []string {
	out := make([]string, len(AllowedTemplates))
	copy(out, AllowedTemplates)
	sort.Strings(out)
	return out
}

// Resolve loads the named template's Dockerfile body. Templates not on
// the allowlist are rejected before the filesystem is touched.
func (d *DiskRegistry) Resolve(name string) (string, error) {
	if !contains(AllowedTemplates, name) {
		return "", fmt.Errorf("template %q is not on the allowlist", name)
	}
	path := filepath.Join(d.root, name, "Dockerfile")
	body, err := os.ReadFile(path) // #nosec G304 -- path is allowlist-gated
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	if len(body) == 0 {
		return "", errors.New("template body is empty")
	}
	return string(body), nil
}

// MemoryRegistry holds template bodies in memory. Used by tests to
// avoid touching disk.
type MemoryRegistry struct {
	bodies map[string]string
}

// NewMemoryRegistry constructs a MemoryRegistry with the given bodies.
// Names not on the AllowedTemplates list are rejected at Resolve time
// even if a body is registered for them — this exercises the same
// security boundary as the disk registry.
func NewMemoryRegistry(bodies map[string]string) *MemoryRegistry {
	cp := make(map[string]string, len(bodies))
	for k, v := range bodies {
		cp[k] = v
	}
	return &MemoryRegistry{bodies: cp}
}

// List returns the canonical template names that also have a body
// registered.
func (m *MemoryRegistry) List() []string {
	out := make([]string, 0, len(m.bodies))
	for _, n := range AllowedTemplates {
		if _, ok := m.bodies[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

// Resolve returns the body for an allowlisted template.
func (m *MemoryRegistry) Resolve(name string) (string, error) {
	if !contains(AllowedTemplates, name) {
		return "", fmt.Errorf("template %q is not on the allowlist", name)
	}
	body, ok := m.bodies[name]
	if !ok {
		return "", fmt.Errorf("template %q has no body registered", name)
	}
	return body, nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

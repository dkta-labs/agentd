package runner

import "fmt"

type Resolver interface {
	Runner(string) (Runner, error)
}

type Registry struct{ runners map[string]Runner }

func NewRegistry(values map[string]Runner) *Registry {
	copied := make(map[string]Runner, len(values))
	for id, r := range values {
		copied[id] = r
	}
	return &Registry{runners: copied}
}
func (r *Registry) Runner(id string) (Runner, error) {
	if value, ok := r.runners[id]; ok {
		return value, nil
	}
	return nil, fmt.Errorf("runner %q is not configured", id)
}

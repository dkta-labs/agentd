package runtime

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

type Capability string

const (
	CapabilityPrompt       Capability = "prompt"
	CapabilitySteer        Capability = "steer"
	CapabilityFollowUp     Capability = "followUp"
	CapabilityAbort        Capability = "abort"
	CapabilityInteractions Capability = "interactions"
	CapabilityToolEvents   Capability = "toolEvents"
	CapabilityReattach     Capability = "reattach"
)

type Descriptor struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Capabilities []Capability `json:"capabilities"`
}

type Registration struct {
	Descriptor Descriptor
	Driver     Driver
	Normalize  EventNormalizer
}

type Registry struct {
	mu        sync.RWMutex
	defaultID string
	entries   map[string]Registration
}

func NewRegistry(defaultID string, registrations ...Registration) (*Registry, error) {
	defaultID = strings.TrimSpace(defaultID)
	if defaultID == "" {
		return nil, errors.New("default harness ID is required")
	}
	registry := &Registry{defaultID: defaultID, entries: make(map[string]Registration, len(registrations))}
	for _, registration := range registrations {
		id := strings.TrimSpace(registration.Descriptor.ID)
		if id == "" || registration.Driver == nil {
			return nil, errors.New("harness registration requires an ID and driver")
		}
		if _, exists := registry.entries[id]; exists {
			return nil, fmt.Errorf("duplicate harness %q", id)
		}
		registration.Descriptor.ID = id
		registry.entries[id] = registration
	}
	if _, exists := registry.entries[defaultID]; !exists {
		return nil, fmt.Errorf("default harness %q is not registered", defaultID)
	}
	return registry, nil
}

func (r *Registry) Resolve(id string) (Registration, error) {
	if r == nil {
		return Registration{}, errors.New("harness registry is unavailable")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		id = r.defaultID
	}
	r.mu.RLock()
	registration, exists := r.entries[id]
	r.mu.RUnlock()
	if !exists {
		return Registration{}, fmt.Errorf("harness %q is not registered", id)
	}
	return registration, nil
}

func (r *Registry) List() []Descriptor {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	result := make([]Descriptor, 0, len(r.entries))
	for _, registration := range r.entries {
		descriptor := registration.Descriptor
		descriptor.Capabilities = append([]Capability(nil), descriptor.Capabilities...)
		result = append(result, descriptor)
	}
	r.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (r *Registry) DefaultID() string {
	if r == nil {
		return ""
	}
	return r.defaultID
}

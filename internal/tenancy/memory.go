/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tenancy

import (
	"context"
	"sync"
)

// MemoryRegistry is an in-process Registry for tests.
type MemoryRegistry struct {
	mu   sync.Mutex
	recs map[string]Record
}

// NewMemoryRegistry returns an empty MemoryRegistry.
func NewMemoryRegistry() *MemoryRegistry {
	return &MemoryRegistry{recs: map[string]Record{}}
}

// GetTenant implements Registry.
func (m *MemoryRegistry) GetTenant(_ context.Context, id string) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[id]
	if !ok {
		return nil, nil
	}
	return &r, nil
}

// PutTenant implements Registry.
func (m *MemoryRegistry) PutTenant(_ context.Context, r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.recs[r.ID]; ok {
		r.CreatedAt = old.CreatedAt
	}
	m.recs[r.ID] = r
	return nil
}

// LiveTenantsByName implements Registry.
func (m *MemoryRegistry) LiveTenantsByName(_ context.Context, namespace, name string) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Record
	for _, r := range m.recs {
		if r.Namespace == namespace && r.Name == name && r.State != StateDeleted {
			out = append(out, r)
		}
	}
	return out, nil
}

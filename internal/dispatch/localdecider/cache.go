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

package localdecider

import (
	"container/list"
	"sync"
)

// lru is a size-bounded cache that evicts what was used longest ago.
type lru[V any] struct {
	mu    sync.Mutex
	limit int
	order *list.List
	items map[[32]byte]*list.Element
}

type entry[V any] struct {
	key   [32]byte
	value V
}

func newLRU[V any](limit int) *lru[V] {
	return &lru[V]{limit: limit, order: list.New(), items: map[[32]byte]*list.Element{}}
}

func (c *lru[V]) get(key [32]byte) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		return el.Value.(entry[V]).value, true
	}
	var zero V
	return zero, false
}

func (c *lru[V]) put(key [32]byte, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value = entry[V]{key, value}
		c.order.MoveToFront(el)
		return
	}
	c.items[key] = c.order.PushFront(entry[V]{key, value})
	for c.order.Len() > c.limit {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(entry[V]).key)
	}
}

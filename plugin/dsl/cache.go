package dsl

import "sync"

// GetOrCompute returns the value for key from cache, or computes it with build() and stores it.
// Uses double-checked locking so compilation/parsing is done once per key.
func GetOrCompute[K comparable, V any](mu *sync.RWMutex, cache *map[K]V, key K, build func() (V, error)) (V, error) {
	mu.RLock()
	v, ok := (*cache)[key]
	mu.RUnlock()
	if ok {
		return v, nil
	}
	mu.Lock()
	defer mu.Unlock()
	if v, ok = (*cache)[key]; ok {
		return v, nil
	}
	v, err := build()
	if err != nil {
		var zero V
		return zero, err
	}
	if *cache == nil {
		*cache = make(map[K]V)
	}
	(*cache)[key] = v
	return v, nil
}

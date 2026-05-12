package arrow

import "sync"

// globalStore is the process-wide shared buffer store.
// Set once at startup via SetGlobalStore.
var (
	globalStore *SharedStore
	globalOnce  sync.Once
)

// SetGlobalStore sets the process-wide shared store. Call once at startup.
func SetGlobalStore(s *SharedStore) {
	globalOnce.Do(func() {
		globalStore = s
	})
}

// GlobalStore returns the process-wide shared store, creating one if needed.
func GlobalStore() *SharedStore {
	if globalStore == nil {
		globalStore = NewSharedStore()
	}
	return globalStore
}

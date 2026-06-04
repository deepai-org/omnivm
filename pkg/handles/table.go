// Package handles manages process-local cross-runtime object handles.
//
// Handles preserve identity for complex values that should not cross runtime
// boundaries as JSON copies. They are scope-bound by default: request/manifest
// cleanup releases non-escaped handles, while proxy finalizers can release
// handles earlier through a safe runtime-owned queue.
package handles

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ID is an opaque process-local handle id.
type ID uint64

// ScopeID identifies a request, manifest execution, or other lifetime scope.
type ScopeID uint64

// ReleaseFunc frees runtime-owned state for a handle. It must be idempotent at
// the runtime adapter layer; Table guarantees it is called at most once per id.
type ReleaseFunc func(value any) error

// Entry describes a live runtime-owned object.
type Entry struct {
	ID          ID
	Runtime     string
	Kind        string
	TypeHint    string
	ScopeID     ScopeID
	StrongRefs  int64
	Finalizers  int64
	Accesses    int64
	Escaped     bool
	Value       any
	Release     ReleaseFunc
	CreatedAt   time.Time
	LastAccess  time.Time
	ReleaseSite string

	accessesByKind      map[string]int64
	chattyAccessWarning bool
	references          map[ID]string
}

// RegisterOptions controls handle creation.
type RegisterOptions struct {
	Runtime  string
	Kind     string
	TypeHint string
	ScopeID  ScopeID
	Release  ReleaseFunc
	Now      time.Time
}

// AccessOptions describes a guest proxy access against a handle. Runtime
// adapters should use generic access kinds such as "property", "index",
// "call", "iterate", "buffer", or "stream".
type AccessOptions struct {
	Kind            string
	Now             time.Time
	ChattyThreshold int64
}

// AccessReport is returned after recording a handle access.
type AccessReport struct {
	ID                       ID
	Runtime                  string
	Kind                     string
	AccessKind               string
	Accesses                 int64
	KindAccesses             int64
	ChattiestAccessKind      string
	ChattiestAccessKindCount int64
	Chatty                   bool
}

// ReferenceReport is returned after recording an edge in the handle graph.
type ReferenceReport struct {
	From ID
	To   ID
	Kind string
}

// Stats is a snapshot for diagnostics and omnivm.status().
type Stats struct {
	Live                     int            `json:"live"`
	Released                 int64          `json:"released"`
	ReleaseErrors            int64          `json:"release_errors"`
	FinalizerReleases        int64          `json:"finalizer_releases"`
	FinalizerQueued          int64          `json:"finalizer_queued"`
	FinalizerQueueDrains     int64          `json:"finalizer_queue_drains"`
	FinalizerQueueDrops      int64          `json:"finalizer_queue_drops"`
	FinalizerQueueLen        int            `json:"finalizer_queue_len"`
	FinalizerOverflowHandles int            `json:"finalizer_overflow_handles"`
	ScopeReleases            int64          `json:"scope_releases"`
	ExplicitReleases         int64          `json:"explicit_releases"`
	StrongRefs               int64          `json:"strong_refs"`
	RetainedRefs             int64          `json:"retained_refs"`
	RetainedHandles          int            `json:"retained_handles"`
	MaxStrongRefs            int64          `json:"max_strong_refs"`
	MaxStrongRefHandleID     ID             `json:"max_strong_ref_handle_id"`
	MaxStrongRefHandleKind   string         `json:"max_strong_ref_handle_kind"`
	HandleAccesses           int64          `json:"handle_accesses"`
	HandleAccessesByKind     map[string]int `json:"handle_accesses_by_kind"`
	ChattyProxyWarnings      int64          `json:"chatty_proxy_warnings"`
	LiveByRuntime            map[string]int `json:"live_by_runtime"`
	EscapedByRuntime         map[string]int `json:"escaped_by_runtime"`
	RetainedByRuntime        map[string]int `json:"retained_by_runtime"`
	ChattyByRuntime          map[string]int `json:"chatty_by_runtime"`
	OldestLiveHandleID       ID             `json:"oldest_live_handle_id"`
	OldestLiveHandleAge      time.Duration  `json:"oldest_live_handle_age_ns"`
	OldestLiveHandleKind     string         `json:"oldest_live_handle_kind"`
	ChattiestHandleID        ID             `json:"chattiest_handle_id"`
	ChattiestAccesses        int64          `json:"chattiest_accesses"`
	ChattiestHandleKind      string         `json:"chattiest_handle_kind"`
	ChattiestAccessKind      string         `json:"chattiest_access_kind"`
	ChattiestAccessKindCount int64          `json:"chattiest_access_kind_count"`
	ReferenceEdges           int            `json:"reference_edges"`
	ReferenceEdgesByKind     map[string]int `json:"reference_edges_by_kind"`
	ReferenceEdgesByRuntime  map[string]int `json:"reference_edges_by_runtime"`
	SuspectedCycles          int            `json:"suspected_cycles"`
	CyclicHandles            int            `json:"cyclic_handles"`
	LargestCycle             int            `json:"largest_cycle"`
	CycleSample              []ID           `json:"cycle_sample,omitempty"`
}

type releaseCause string

const (
	causeExplicit  releaseCause = "explicit"
	causeFinalizer releaseCause = "finalizer"
	causeScope     releaseCause = "scope"

	finalizerQueueCapacity    = 4096
	finalizerSpillHandleLimit = 4096
)

// Table stores live handles. It is safe for concurrent use.
type Table struct {
	mu             sync.Mutex
	nextID         ID
	nextSID        ScopeID
	entries        map[ID]*Entry
	finalizerQueue chan ID
	finalizerSpill map[ID]int64
	finalizerOrder []ID
	finalizerTotal int

	released          int64
	releaseErrors     int64
	finalizerReleases int64
	finalizerQueued   int64
	finalizerDrains   int64
	finalizerDrops    int64
	scopeReleases     int64
	explicitReleases  int64
	handleAccesses    int64
	chattyWarnings    int64
	accessesByKind    map[string]int64
}

// NewTable creates an empty handle table.
func NewTable() *Table {
	return &Table{
		nextID:         1,
		nextSID:        1,
		entries:        make(map[ID]*Entry),
		finalizerQueue: make(chan ID, finalizerQueueCapacity),
		finalizerSpill: make(map[ID]int64),
		accessesByKind: make(map[string]int64),
	}
}

// NewScope reserves a fresh lifetime scope id.
func (t *Table) NewScope() ScopeID {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := t.nextSID
	t.nextSID++
	return id
}

// Register stores a new handle with one strong reference.
func (t *Table) Register(value any, opts RegisterOptions) (ID, error) {
	if opts.Runtime == "" {
		return 0, errors.New("handles: runtime is required")
	}
	if opts.Kind == "" {
		return 0, errors.New("handles: kind is required")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	id := t.nextID
	t.nextID++
	t.entries[id] = &Entry{
		ID:             id,
		Runtime:        opts.Runtime,
		Kind:           opts.Kind,
		TypeHint:       opts.TypeHint,
		ScopeID:        opts.ScopeID,
		StrongRefs:     1,
		Value:          value,
		Release:        opts.Release,
		CreatedAt:      now,
		LastAccess:     now,
		accessesByKind: make(map[string]int64),
		references:     make(map[ID]string),
	}
	return id, nil
}

// Get returns a copy of the live entry and marks it accessed.
func (t *Table) Get(id ID) (Entry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.entries[id]
	if !ok {
		return Entry{}, false
	}
	t.recordAccessLocked(entry, AccessOptions{Kind: "get", Now: time.Now()})
	return *entry, true
}

// RecordAccess records a guest proxy operation against a handle. It is the
// generic hook used by runtime adapters to detect repeated cross-boundary
// property/index/call traffic without introducing user-visible APIs.
func (t *Table) RecordAccess(id ID, opts AccessOptions) (AccessReport, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.entries[id]
	if !ok {
		return AccessReport{}, fmt.Errorf("handles: unknown handle %d", id)
	}
	chatty := t.recordAccessLocked(entry, opts)
	kind := opts.Kind
	if kind == "" {
		kind = "access"
	}
	chattiestKind, chattiestCount := chattiestAccessKind(entry.accessesByKind)
	return AccessReport{
		ID:                       entry.ID,
		Runtime:                  entry.Runtime,
		Kind:                     entry.Kind,
		AccessKind:               kind,
		Accesses:                 entry.Accesses,
		KindAccesses:             entry.accessesByKind[kind],
		ChattiestAccessKind:      chattiestKind,
		ChattiestAccessKindCount: chattiestCount,
		Chatty:                   chatty,
	}, nil
}

// RecordReference records that one live handle references another. Runtime
// adapters use this to make cross-runtime proxy graphs observable without
// encoding producer-specific ownership rules in the handle table.
func (t *Table) RecordReference(from, to ID, kind string) (ReferenceReport, error) {
	if kind == "" {
		kind = "reference"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	source, ok := t.entries[from]
	if !ok {
		return ReferenceReport{}, fmt.Errorf("handles: unknown source handle %d", from)
	}
	if _, ok := t.entries[to]; !ok {
		return ReferenceReport{}, fmt.Errorf("handles: unknown target handle %d", to)
	}
	if source.references == nil {
		source.references = make(map[ID]string)
	}
	source.references[to] = kind
	source.LastAccess = time.Now()
	return ReferenceReport{From: from, To: to, Kind: kind}, nil
}

// DropReference removes a previously observed handle graph edge. It is
// idempotent so finalizer/scope cleanup paths can race safely.
func (t *Table) DropReference(from, to ID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if source, ok := t.entries[from]; ok {
		delete(source.references, to)
		source.LastAccess = time.Now()
	}
}

// Retain increments the strong ref count.
func (t *Table) Retain(id ID) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.entries[id]
	if !ok {
		return fmt.Errorf("handles: unknown handle %d", id)
	}
	entry.StrongRefs++
	t.recordAccessLocked(entry, AccessOptions{Kind: "retain", Now: time.Now()})
	return nil
}

// Escape marks a handle as intentionally outliving its creating scope.
func (t *Table) Escape(id ID) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.entries[id]
	if !ok {
		return fmt.Errorf("handles: unknown handle %d", id)
	}
	entry.Escaped = true
	t.recordAccessLocked(entry, AccessOptions{Kind: "escape", Now: time.Now()})
	return nil
}

// Release drops one strong reference. The release callback runs when the ref
// count reaches zero. Releasing an already-released id is a no-op.
func (t *Table) Release(id ID) error {
	return t.release(id, causeExplicit)
}

// ReleaseAllRefs drops all strong references for a live handle. Stream
// exhaustion and cancellation use this because the underlying producer is no
// longer usable even if guest proxy finalizers have retained extra refs.
func (t *Table) ReleaseAllRefs(id ID) error {
	return t.releaseAll(id, causeExplicit)
}

// ReleaseFromFinalizer drops one strong reference due to a guest proxy GC event.
func (t *Table) ReleaseFromFinalizer(id ID) error {
	t.mu.Lock()
	if entry, ok := t.entries[id]; ok {
		entry.Finalizers++
	}
	t.mu.Unlock()
	return t.release(id, causeFinalizer)
}

// QueueReleaseFromFinalizer records a guest GC finalizer release without
// running release callbacks on a runtime-owned finalizer thread. The owning
// runtime or host should call DrainFinalizerReleases from a safe thread.
func (t *Table) QueueReleaseFromFinalizer(id ID) bool {
	t.mu.Lock()
	entry, ok := t.entries[id]
	if !ok {
		t.mu.Unlock()
		return false
	}
	entry.Finalizers++
	t.mu.Unlock()

	select {
	case t.finalizerQueue <- id:
		t.mu.Lock()
		t.finalizerQueued++
		t.mu.Unlock()
		return true
	default:
		t.mu.Lock()
		if t.finalizerSpill[id] == 0 {
			if len(t.finalizerSpill) >= finalizerSpillHandleLimit {
				t.finalizerDrops++
				t.mu.Unlock()
				return false
			}
			t.finalizerOrder = append(t.finalizerOrder, id)
		}
		t.finalizerSpill[id]++
		t.finalizerTotal++
		t.finalizerQueued++
		t.mu.Unlock()
		return true
	}
}

// DrainFinalizerReleases releases queued finalizer refs from a safe host-owned
// context. If max is <= 0, it drains until the queue is empty.
func (t *Table) DrainFinalizerReleases(max int) error {
	drained := 0
	var firstErr error
	for max <= 0 || drained < max {
		id, ok := t.nextQueuedFinalizerRelease()
		if !ok {
			return firstErr
		}
		t.mu.Lock()
		t.finalizerDrains++
		t.mu.Unlock()
		if err := t.release(id, causeFinalizer); err != nil && firstErr == nil {
			firstErr = err
		}
		drained++
	}
	return firstErr
}

// ReleaseScope releases every non-escaped handle created in scope.
func (t *Table) ReleaseScope(scope ScopeID) error {
	var ids []ID
	t.mu.Lock()
	for id, entry := range t.entries {
		if entry.ScopeID == scope && !entry.Escaped {
			ids = append(ids, id)
		}
	}
	t.mu.Unlock()

	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var firstErr error
	for _, id := range ids {
		if err := t.releaseAll(id, causeScope); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ReleaseAll releases every live handle, including escaped handles. It is used
// during runtime shutdown or explicit worker drain/reload cleanup when no proxy
// can safely dereference process-local ids afterward.
func (t *Table) ReleaseAll() error {
	var ids []ID
	t.mu.Lock()
	for id := range t.entries {
		ids = append(ids, id)
	}
	t.mu.Unlock()

	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var firstErr error
	for _, id := range ids {
		if err := t.releaseAll(id, causeScope); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Stats returns a diagnostics snapshot.
func (t *Table) Stats(now time.Time) Stats {
	if now.IsZero() {
		now = time.Now()
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	stats := Stats{
		Live:                     len(t.entries),
		Released:                 t.released,
		ReleaseErrors:            t.releaseErrors,
		FinalizerReleases:        t.finalizerReleases,
		FinalizerQueued:          t.finalizerQueued,
		FinalizerQueueDrains:     t.finalizerDrains,
		FinalizerQueueDrops:      t.finalizerDrops,
		FinalizerQueueLen:        len(t.finalizerQueue) + t.finalizerTotal,
		FinalizerOverflowHandles: len(t.finalizerSpill),
		ScopeReleases:            t.scopeReleases,
		ExplicitReleases:         t.explicitReleases,
		HandleAccesses:           t.handleAccesses,
		ChattyProxyWarnings:      t.chattyWarnings,
		HandleAccessesByKind:     make(map[string]int),
		LiveByRuntime:            make(map[string]int),
		EscapedByRuntime:         make(map[string]int),
		RetainedByRuntime:        make(map[string]int),
		ChattyByRuntime:          make(map[string]int),
		ReferenceEdgesByKind:     make(map[string]int),
		ReferenceEdgesByRuntime:  make(map[string]int),
	}
	for kind, count := range t.accessesByKind {
		stats.HandleAccessesByKind[kind] = int(count)
	}
	for _, entry := range t.entries {
		stats.LiveByRuntime[entry.Runtime]++
		stats.StrongRefs += entry.StrongRefs
		if entry.StrongRefs > 1 {
			stats.RetainedHandles++
			stats.RetainedRefs += entry.StrongRefs - 1
			stats.RetainedByRuntime[entry.Runtime]++
		}
		if entry.StrongRefs > stats.MaxStrongRefs {
			stats.MaxStrongRefs = entry.StrongRefs
			stats.MaxStrongRefHandleID = entry.ID
			stats.MaxStrongRefHandleKind = entry.Kind
		}
		if entry.Escaped {
			stats.EscapedByRuntime[entry.Runtime]++
		}
		if entry.chattyAccessWarning {
			stats.ChattyByRuntime[entry.Runtime]++
		}
		if stats.OldestLiveHandleID == 0 || entry.CreatedAt.Before(now.Add(-stats.OldestLiveHandleAge)) {
			stats.OldestLiveHandleID = entry.ID
			stats.OldestLiveHandleAge = now.Sub(entry.CreatedAt)
			stats.OldestLiveHandleKind = entry.Kind
		}
		if entry.Accesses > stats.ChattiestAccesses {
			stats.ChattiestHandleID = entry.ID
			stats.ChattiestAccesses = entry.Accesses
			stats.ChattiestHandleKind = entry.Kind
			stats.ChattiestAccessKind, stats.ChattiestAccessKindCount = chattiestAccessKind(entry.accessesByKind)
		}
		for targetID, kind := range entry.references {
			target, ok := t.entries[targetID]
			if !ok {
				continue
			}
			stats.ReferenceEdges++
			stats.ReferenceEdgesByKind[kind]++
			stats.ReferenceEdgesByRuntime[entry.Runtime+"->"+target.Runtime]++
		}
	}
	stats.SuspectedCycles, stats.CyclicHandles, stats.LargestCycle, stats.CycleSample = t.cycleStatsLocked()
	return stats
}

func (t *Table) nextQueuedFinalizerRelease() (ID, bool) {
	select {
	case id := <-t.finalizerQueue:
		return id, true
	default:
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.finalizerOrder) == 0 {
		return 0, false
	}
	id := t.finalizerOrder[0]
	t.finalizerSpill[id]--
	t.finalizerTotal--
	if t.finalizerSpill[id] <= 0 {
		delete(t.finalizerSpill, id)
		copy(t.finalizerOrder, t.finalizerOrder[1:])
		t.finalizerOrder[len(t.finalizerOrder)-1] = 0
		t.finalizerOrder = t.finalizerOrder[:len(t.finalizerOrder)-1]
	}
	return id, true
}

func chattiestAccessKind(counts map[string]int64) (string, int64) {
	var winner string
	var winnerCount int64
	for kind, count := range counts {
		if !actionableProxyAccessKind(kind) {
			continue
		}
		if count > winnerCount || (count == winnerCount && (winner == "" || kind < winner)) {
			winner = kind
			winnerCount = count
		}
	}
	return winner, winnerCount
}

func actionableProxyAccesses(counts map[string]int64) int64 {
	var total int64
	for kind, count := range counts {
		if actionableProxyAccessKind(kind) {
			total += count
		}
	}
	return total
}

func actionableProxyAccessKind(kind string) bool {
	switch kind {
	case "", "get", "retain", "escape":
		return false
	default:
		return true
	}
}

func (t *Table) release(id ID, cause releaseCause) error {
	var entry *Entry
	t.mu.Lock()
	entry = t.entries[id]
	if entry == nil {
		t.mu.Unlock()
		return nil
	}
	entry.StrongRefs--
	if entry.StrongRefs > 0 {
		entry.LastAccess = time.Now()
		t.mu.Unlock()
		return nil
	}
	t.dropReferencesLocked(id)
	delete(t.entries, id)
	t.recordReleaseLocked(cause)
	t.mu.Unlock()
	return t.callRelease(entry, cause)
}

func (t *Table) releaseAll(id ID, cause releaseCause) error {
	var entry *Entry
	t.mu.Lock()
	entry = t.entries[id]
	if entry == nil {
		t.mu.Unlock()
		return nil
	}
	t.dropReferencesLocked(id)
	delete(t.entries, id)
	t.recordReleaseLocked(cause)
	t.mu.Unlock()
	return t.callRelease(entry, cause)
}

func (t *Table) recordReleaseLocked(cause releaseCause) {
	t.released++
	switch cause {
	case causeFinalizer:
		t.finalizerReleases++
	case causeScope:
		t.scopeReleases++
	case causeExplicit:
		t.explicitReleases++
	}
}

func (t *Table) callRelease(entry *Entry, cause releaseCause) error {
	if entry.Release == nil {
		return nil
	}
	if err := entry.Release(entry.Value); err != nil {
		t.mu.Lock()
		t.releaseErrors++
		t.mu.Unlock()
		return fmt.Errorf("handles: release %d via %s: %w", entry.ID, cause, err)
	}
	return nil
}

func (t *Table) recordAccessLocked(entry *Entry, opts AccessOptions) bool {
	kind := opts.Kind
	if kind == "" {
		kind = "access"
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	threshold := opts.ChattyThreshold
	if threshold <= 0 {
		threshold = 64
	}

	entry.Accesses++
	entry.LastAccess = now
	if entry.accessesByKind == nil {
		entry.accessesByKind = make(map[string]int64)
	}
	entry.accessesByKind[kind]++
	t.handleAccesses++
	if t.accessesByKind == nil {
		t.accessesByKind = make(map[string]int64)
	}
	t.accessesByKind[kind]++

	if !entry.chattyAccessWarning && actionableProxyAccesses(entry.accessesByKind) >= threshold {
		entry.chattyAccessWarning = true
		t.chattyWarnings++
		return true
	}
	return entry.chattyAccessWarning
}

func (t *Table) dropReferencesLocked(id ID) {
	for _, entry := range t.entries {
		delete(entry.references, id)
	}
}

func (t *Table) cycleStatsLocked() (cycles int, cyclicHandles int, largest int, sample []ID) {
	index := 0
	indexes := make(map[ID]int, len(t.entries))
	lowlink := make(map[ID]int, len(t.entries))
	onStack := make(map[ID]bool, len(t.entries))
	var stack []ID

	var ids []ID
	for id := range t.entries {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var strongConnect func(ID)
	strongConnect = func(id ID) {
		indexes[id] = index
		lowlink[id] = index
		index++
		stack = append(stack, id)
		onStack[id] = true

		var targets []ID
		for targetID := range t.entries[id].references {
			if _, ok := t.entries[targetID]; ok {
				targets = append(targets, targetID)
			}
		}
		sort.Slice(targets, func(i, j int) bool { return targets[i] < targets[j] })
		for _, targetID := range targets {
			if _, seen := indexes[targetID]; !seen {
				strongConnect(targetID)
				if lowlink[targetID] < lowlink[id] {
					lowlink[id] = lowlink[targetID]
				}
			} else if onStack[targetID] && indexes[targetID] < lowlink[id] {
				lowlink[id] = indexes[targetID]
			}
		}

		if lowlink[id] != indexes[id] {
			return
		}

		var component []ID
		for {
			last := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[last] = false
			component = append(component, last)
			if last == id {
				break
			}
		}
		sort.Slice(component, func(i, j int) bool { return component[i] < component[j] })
		if len(component) > 1 || t.entries[id].references[id] != "" {
			cycles++
			cyclicHandles += len(component)
			if len(component) > largest {
				largest = len(component)
				sample = append([]ID(nil), component...)
			}
		}
	}

	for _, id := range ids {
		if _, seen := indexes[id]; !seen {
			strongConnect(id)
		}
	}
	return cycles, cyclicHandles, largest, sample
}

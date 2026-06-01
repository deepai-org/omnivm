package handles

import (
	"errors"
	"testing"
	"time"
)

func TestRegisterRequiresRuntimeAndKind(t *testing.T) {
	table := NewTable()
	if _, err := table.Register("value", RegisterOptions{Kind: "object"}); err == nil {
		t.Fatal("expected missing runtime error")
	}
	if _, err := table.Register("value", RegisterOptions{Runtime: "python"}); err == nil {
		t.Fatal("expected missing kind error")
	}
}

func TestRegisterGetRetainRelease(t *testing.T) {
	table := NewTable()
	scope := table.NewScope()
	releases := 0
	id, err := table.Register("payload", RegisterOptions{
		Runtime: "python",
		Kind:    "object",
		ScopeID: scope,
		Release: func(value any) error {
			releases++
			if value != "payload" {
				t.Fatalf("release value = %v, want payload", value)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	entry, ok := table.Get(id)
	if !ok {
		t.Fatal("registered handle not found")
	}
	if entry.Runtime != "python" || entry.Kind != "object" || entry.ScopeID != scope {
		t.Fatalf("bad entry: %+v", entry)
	}

	if err := table.Retain(id); err != nil {
		t.Fatal(err)
	}
	if err := table.Release(id); err != nil {
		t.Fatal(err)
	}
	if releases != 0 {
		t.Fatalf("release called early: %d", releases)
	}
	if err := table.Release(id); err != nil {
		t.Fatal(err)
	}
	if releases != 1 {
		t.Fatalf("release calls = %d, want 1", releases)
	}
	if err := table.Release(id); err != nil {
		t.Fatal(err)
	}
	if releases != 1 {
		t.Fatalf("idempotent release calls = %d, want 1", releases)
	}
}

func TestReleaseScopeReleasesNonEscapedHandles(t *testing.T) {
	table := NewTable()
	scope := table.NewScope()
	released := map[string]bool{}
	id1, err := table.Register("one", RegisterOptions{
		Runtime: "python",
		Kind:    "request",
		ScopeID: scope,
		Release: func(value any) error {
			released[value.(string)] = true
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := table.Register("two", RegisterOptions{
		Runtime: "javascript",
		Kind:    "proxy",
		ScopeID: scope,
		Release: func(value any) error {
			released[value.(string)] = true
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := table.Escape(id2); err != nil {
		t.Fatal(err)
	}

	if err := table.ReleaseScope(scope); err != nil {
		t.Fatal(err)
	}
	if !released["one"] || released["two"] {
		t.Fatalf("released = %#v, want only one", released)
	}
	if _, ok := table.Get(id1); ok {
		t.Fatal("non-escaped handle survived scope release")
	}
	if _, ok := table.Get(id2); !ok {
		t.Fatal("escaped handle did not survive scope release")
	}
	if err := table.Release(id2); err != nil {
		t.Fatal(err)
	}
	if !released["two"] {
		t.Fatal("escaped handle was not released explicitly")
	}
}

func TestReleaseAllReleasesEscapedHandles(t *testing.T) {
	table := NewTable()
	scope := table.NewScope()
	released := 0
	id, err := table.Register("value", RegisterOptions{
		Runtime: "python",
		Kind:    "object",
		ScopeID: scope,
		Release: func(value any) error {
			released++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := table.Escape(id); err != nil {
		t.Fatal(err)
	}
	if err := table.ReleaseAll(); err != nil {
		t.Fatal(err)
	}
	if released != 1 {
		t.Fatalf("release calls = %d, want 1", released)
	}
	if stats := table.Stats(time.Now()); stats.Live != 0 {
		t.Fatalf("live = %d, want 0", stats.Live)
	}
}

func TestFinalizerReleaseUpdatesStats(t *testing.T) {
	table := NewTable()
	id, err := table.Register("value", RegisterOptions{Runtime: "javascript", Kind: "proxy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := table.ReleaseFromFinalizer(id); err != nil {
		t.Fatal(err)
	}
	stats := table.Stats(time.Now())
	if stats.Live != 0 || stats.Released != 1 || stats.FinalizerReleases != 1 {
		t.Fatalf("bad stats: %+v", stats)
	}
}

func TestQueueReleaseFromFinalizerDrainsOnSafeThread(t *testing.T) {
	table := NewTable()
	releases := 0
	id, err := table.Register("value", RegisterOptions{
		Runtime: "javascript",
		Kind:    "proxy",
		Release: func(value any) error {
			releases++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if ok := table.QueueReleaseFromFinalizer(id); !ok {
		t.Fatal("expected finalizer release to be queued")
	}
	if releases != 0 {
		t.Fatalf("release ran before drain: %d", releases)
	}
	stats := table.Stats(time.Now())
	if stats.Live != 1 || stats.FinalizerQueued != 1 || stats.FinalizerQueueLen != 1 {
		t.Fatalf("bad queued finalizer stats before drain: %+v", stats)
	}

	if err := table.DrainFinalizerReleases(0); err != nil {
		t.Fatal(err)
	}
	if releases != 1 {
		t.Fatalf("release calls after drain = %d, want 1", releases)
	}
	stats = table.Stats(time.Now())
	if stats.Live != 0 || stats.FinalizerReleases != 1 || stats.FinalizerQueueDrains != 1 || stats.FinalizerQueueLen != 0 {
		t.Fatalf("bad queued finalizer stats after drain: %+v", stats)
	}
}

func TestQueueReleaseFromFinalizerIgnoresUnknownHandle(t *testing.T) {
	table := NewTable()
	if ok := table.QueueReleaseFromFinalizer(999); ok {
		t.Fatal("unknown handle should not be queued")
	}
	stats := table.Stats(time.Now())
	if stats.FinalizerQueued != 0 || stats.FinalizerQueueDrops != 0 || stats.FinalizerQueueLen != 0 {
		t.Fatalf("bad stats for unknown finalizer release: %+v", stats)
	}
}

func TestDrainFinalizerReleasesHonorsMax(t *testing.T) {
	table := NewTable()
	releases := 0
	id1, err := table.Register("one", RegisterOptions{
		Runtime: "python",
		Kind:    "proxy",
		Release: func(value any) error {
			releases++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := table.Register("two", RegisterOptions{
		Runtime: "python",
		Kind:    "proxy",
		Release: func(value any) error {
			releases++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !table.QueueReleaseFromFinalizer(id1) || !table.QueueReleaseFromFinalizer(id2) {
		t.Fatal("expected both finalizer releases to queue")
	}

	if err := table.DrainFinalizerReleases(1); err != nil {
		t.Fatal(err)
	}
	stats := table.Stats(time.Now())
	if releases != 1 || stats.Live != 1 || stats.FinalizerQueueDrains != 1 || stats.FinalizerQueueLen != 1 {
		t.Fatalf("bad stats after partial drain: releases=%d stats=%+v", releases, stats)
	}
	if err := table.DrainFinalizerReleases(1); err != nil {
		t.Fatal(err)
	}
	stats = table.Stats(time.Now())
	if releases != 2 || stats.Live != 0 || stats.FinalizerQueueDrains != 2 || stats.FinalizerQueueLen != 0 {
		t.Fatalf("bad stats after second drain: releases=%d stats=%+v", releases, stats)
	}
}

func TestQueueReleaseFromFinalizerSpillsWhenChannelIsFull(t *testing.T) {
	table := NewTable()
	capacity := cap(table.finalizerQueue)
	releases := 0
	for i := 0; i < capacity+1; i++ {
		id, err := table.Register(i, RegisterOptions{
			Runtime: "javascript",
			Kind:    "proxy",
			Release: func(value any) error {
				releases++
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if ok := table.QueueReleaseFromFinalizer(id); !ok {
			t.Fatalf("finalizer release %d was dropped", i)
		}
	}

	stats := table.Stats(time.Now())
	if stats.FinalizerQueued != int64(capacity+1) || stats.FinalizerQueueDrops != 0 || stats.FinalizerQueueLen != capacity+1 {
		t.Fatalf("bad saturated finalizer queue stats: %+v", stats)
	}

	if err := table.DrainFinalizerReleases(0); err != nil {
		t.Fatal(err)
	}
	stats = table.Stats(time.Now())
	if releases != capacity+1 || stats.Live != 0 || stats.FinalizerReleases != int64(capacity+1) || stats.FinalizerQueueLen != 0 || stats.FinalizerQueueDrops != 0 {
		t.Fatalf("spill queue was not fully drained: releases=%d stats=%+v", releases, stats)
	}
}

func TestQueueReleaseFromFinalizerBoundsDistinctSpillHandles(t *testing.T) {
	table := NewTable()
	capacity := cap(table.finalizerQueue)
	accepted := capacity + finalizerSpillHandleLimit
	for i := 0; i < accepted; i++ {
		id, err := table.Register(i, RegisterOptions{Runtime: "javascript", Kind: "proxy"})
		if err != nil {
			t.Fatal(err)
		}
		if ok := table.QueueReleaseFromFinalizer(id); !ok {
			t.Fatalf("finalizer release %d was dropped before spill limit", i)
		}
	}

	id, err := table.Register("dropped", RegisterOptions{Runtime: "javascript", Kind: "proxy"})
	if err != nil {
		t.Fatal(err)
	}
	if ok := table.QueueReleaseFromFinalizer(id); ok {
		t.Fatal("expected distinct overflow handle to be dropped")
	}

	stats := table.Stats(time.Now())
	if stats.FinalizerQueued != int64(accepted) || stats.FinalizerQueueDrops != 1 {
		t.Fatalf("bad bounded spill stats: %+v", stats)
	}
	if stats.FinalizerQueueLen != accepted || stats.FinalizerOverflowHandles != finalizerSpillHandleLimit {
		t.Fatalf("spill queue was not bounded: %+v", stats)
	}
}

func TestQueueReleaseFromFinalizerCoalescesRepeatedSpillIDs(t *testing.T) {
	table := NewTable()
	for i := 0; i < cap(table.finalizerQueue); i++ {
		id, err := table.Register(i, RegisterOptions{Runtime: "javascript", Kind: "proxy"})
		if err != nil {
			t.Fatal(err)
		}
		if ok := table.QueueReleaseFromFinalizer(id); !ok {
			t.Fatalf("filler finalizer release %d was dropped", i)
		}
	}

	releases := 0
	id, err := table.Register("repeated", RegisterOptions{
		Runtime: "javascript",
		Kind:    "proxy",
		Release: func(value any) error {
			releases++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if ok := table.QueueReleaseFromFinalizer(id); !ok {
			t.Fatalf("repeated finalizer release %d was dropped", i)
		}
	}

	stats := table.Stats(time.Now())
	if stats.FinalizerQueueLen != cap(table.finalizerQueue)+10 || stats.FinalizerOverflowHandles != 1 {
		t.Fatalf("bad queue length for repeated spill ids: %+v", stats)
	}
	if len(table.finalizerOrder) != 1 || table.finalizerSpill[id] != 10 {
		t.Fatalf("repeated spill ids were not coalesced: order=%v spill=%v", table.finalizerOrder, table.finalizerSpill)
	}

	if err := table.DrainFinalizerReleases(0); err != nil {
		t.Fatal(err)
	}
	stats = table.Stats(time.Now())
	if releases != 1 || stats.FinalizerQueueLen != 0 || stats.FinalizerOverflowHandles != 0 || stats.FinalizerQueueDrains != int64(cap(table.finalizerQueue)+10) {
		t.Fatalf("coalesced spill did not drain all events: releases=%d stats=%+v", releases, stats)
	}
}

func TestRecordAccessUpdatesStatsAndWarnsOnce(t *testing.T) {
	table := NewTable()
	id, err := table.Register("value", RegisterOptions{Runtime: "javascript", Kind: "array"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := table.Get(id); !ok {
		t.Fatal("registered handle not found")
	}

	for i := 0; i < 2; i++ {
		report, err := table.RecordAccess(id, AccessOptions{Kind: "index", ChattyThreshold: 3})
		if err != nil {
			t.Fatal(err)
		}
		if report.Chatty {
			t.Fatalf("access %d reported chatty early: %+v", i+1, report)
		}
	}
	report, err := table.RecordAccess(id, AccessOptions{Kind: "index", ChattyThreshold: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Chatty || report.Accesses != 4 {
		t.Fatalf("third access report = %+v, want chatty at 3 proxy accesses", report)
	}
	if report.AccessKind != "index" || report.KindAccesses != 3 || report.ChattiestAccessKind != "index" || report.ChattiestAccessKindCount != 3 {
		t.Fatalf("third access report did not include dominant access kind: %+v", report)
	}
	report, err = table.RecordAccess(id, AccessOptions{Kind: "property", ChattyThreshold: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Chatty || report.Accesses != 5 {
		t.Fatalf("fourth access report = %+v, want still chatty at 4 accesses", report)
	}
	if report.AccessKind != "property" || report.KindAccesses != 1 || report.ChattiestAccessKind != "index" || report.ChattiestAccessKindCount != 3 {
		t.Fatalf("fourth access report did not preserve dominant access kind: %+v", report)
	}

	stats := table.Stats(time.Now())
	if stats.HandleAccesses != 5 || stats.ChattyProxyWarnings != 1 {
		t.Fatalf("bad access stats: %+v", stats)
	}
	if stats.HandleAccessesByKind["get"] != 1 || stats.HandleAccessesByKind["index"] != 3 || stats.HandleAccessesByKind["property"] != 1 {
		t.Fatalf("bad access stats by kind: %#v", stats.HandleAccessesByKind)
	}
	if stats.ChattyByRuntime["javascript"] != 1 {
		t.Fatalf("bad chatty by runtime: %#v", stats.ChattyByRuntime)
	}
	if stats.ChattiestHandleID != id || stats.ChattiestAccesses != 5 || stats.ChattiestHandleKind != "array" {
		t.Fatalf("bad chattiest stats: %+v", stats)
	}
	if stats.ChattiestAccessKind != "index" || stats.ChattiestAccessKindCount != 3 {
		t.Fatalf("bad chattiest access kind stats: %+v", stats)
	}
	if err := table.Release(id); err != nil {
		t.Fatal(err)
	}
	stats = table.Stats(time.Now())
	if stats.Live != 0 || stats.HandleAccessesByKind["index"] != 3 || stats.HandleAccessesByKind["property"] != 1 {
		t.Fatalf("released handle should retain aggregate access stats: %+v", stats)
	}
}

func TestGetRetainAndEscapeAreRecordedAsGenericAccess(t *testing.T) {
	table := NewTable()
	id, err := table.Register("value", RegisterOptions{Runtime: "python", Kind: "object"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := table.Get(id); !ok {
		t.Fatal("registered handle not found")
	}
	if err := table.Retain(id); err != nil {
		t.Fatal(err)
	}
	if err := table.Escape(id); err != nil {
		t.Fatal(err)
	}

	stats := table.Stats(time.Now())
	if stats.HandleAccesses != 3 {
		t.Fatalf("handle accesses = %d, want 3", stats.HandleAccesses)
	}
	if stats.HandleAccessesByKind["get"] != 1 || stats.HandleAccessesByKind["retain"] != 1 || stats.HandleAccessesByKind["escape"] != 1 {
		t.Fatalf("bad access kinds: %#v", stats.HandleAccessesByKind)
	}
	if stats.StrongRefs != 2 || stats.RetainedRefs != 1 || stats.RetainedHandles != 1 {
		t.Fatalf("bad retained-ref stats: %+v", stats)
	}
	if stats.MaxStrongRefs != 2 || stats.MaxStrongRefHandleID != id || stats.MaxStrongRefHandleKind != "object" {
		t.Fatalf("bad max strong-ref stats: %+v", stats)
	}
	if stats.RetainedByRuntime["python"] != 1 {
		t.Fatalf("bad retained runtime stats: %#v", stats.RetainedByRuntime)
	}
}

func TestRecordReferenceReportsGraphEdgesAndCycles(t *testing.T) {
	table := NewTable()
	pyID, err := table.Register("py", RegisterOptions{Runtime: "python", Kind: "object"})
	if err != nil {
		t.Fatal(err)
	}
	jsID, err := table.Register("js", RegisterOptions{Runtime: "javascript", Kind: "proxy"})
	if err != nil {
		t.Fatal(err)
	}

	report, err := table.RecordReference(pyID, jsID, "proxy")
	if err != nil {
		t.Fatal(err)
	}
	if report.From != pyID || report.To != jsID || report.Kind != "proxy" {
		t.Fatalf("bad reference report: %+v", report)
	}

	stats := table.Stats(time.Now())
	if stats.ReferenceEdges != 1 || stats.ReferenceEdgesByKind["proxy"] != 1 {
		t.Fatalf("bad reference edge stats: %+v", stats)
	}
	if stats.ReferenceEdgesByRuntime["python->javascript"] != 1 {
		t.Fatalf("bad reference runtime stats: %#v", stats.ReferenceEdgesByRuntime)
	}
	if stats.SuspectedCycles != 0 || stats.CyclicHandles != 0 {
		t.Fatalf("unexpected cycle stats before cycle: %+v", stats)
	}

	if _, err := table.RecordReference(jsID, pyID, "proxy"); err != nil {
		t.Fatal(err)
	}
	stats = table.Stats(time.Now())
	if stats.ReferenceEdges != 2 || stats.SuspectedCycles != 1 || stats.CyclicHandles != 2 || stats.LargestCycle != 2 {
		t.Fatalf("bad cycle stats: %+v", stats)
	}
	if len(stats.CycleSample) != 2 || stats.CycleSample[0] != pyID || stats.CycleSample[1] != jsID {
		t.Fatalf("bad cycle sample: %#v", stats.CycleSample)
	}
}

func TestDropReferenceAndReleaseRemoveGraphEdges(t *testing.T) {
	table := NewTable()
	id1, err := table.Register("one", RegisterOptions{Runtime: "python", Kind: "object"})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := table.Register("two", RegisterOptions{Runtime: "javascript", Kind: "proxy"})
	if err != nil {
		t.Fatal(err)
	}
	id3, err := table.Register("three", RegisterOptions{Runtime: "ruby", Kind: "proxy"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := table.RecordReference(id1, id2, "proxy"); err != nil {
		t.Fatal(err)
	}
	if _, err := table.RecordReference(id3, id2, "proxy"); err != nil {
		t.Fatal(err)
	}

	table.DropReference(id1, id2)
	stats := table.Stats(time.Now())
	if stats.ReferenceEdges != 1 || stats.ReferenceEdgesByRuntime["ruby->javascript"] != 1 {
		t.Fatalf("bad graph stats after drop: %+v", stats)
	}

	if err := table.Release(id2); err != nil {
		t.Fatal(err)
	}
	stats = table.Stats(time.Now())
	if stats.ReferenceEdges != 0 {
		t.Fatalf("release should remove inbound graph edges, stats=%+v", stats)
	}
}

func TestRecordReferenceValidatesLiveHandles(t *testing.T) {
	table := NewTable()
	id, err := table.Register("value", RegisterOptions{Runtime: "python", Kind: "object"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := table.RecordReference(id, 999, "proxy"); err == nil {
		t.Fatal("expected unknown target error")
	}
	if _, err := table.RecordReference(999, id, "proxy"); err == nil {
		t.Fatal("expected unknown source error")
	}
}

func TestStatsReportsLiveHandlesByRuntimeAndOldest(t *testing.T) {
	table := NewTable()
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	oldID, err := table.Register("old", RegisterOptions{
		Runtime: "python",
		Kind:    "request",
		Now:     now.Add(-10 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	newID, err := table.Register("new", RegisterOptions{
		Runtime: "javascript",
		Kind:    "array",
		Now:     now.Add(-1 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := table.Escape(newID); err != nil {
		t.Fatal(err)
	}

	stats := table.Stats(now)
	if stats.Live != 2 {
		t.Fatalf("live = %d, want 2", stats.Live)
	}
	if stats.LiveByRuntime["python"] != 1 || stats.LiveByRuntime["javascript"] != 1 {
		t.Fatalf("bad live by runtime: %#v", stats.LiveByRuntime)
	}
	if stats.EscapedByRuntime["javascript"] != 1 {
		t.Fatalf("bad escaped by runtime: %#v", stats.EscapedByRuntime)
	}
	if stats.OldestLiveHandleID != oldID || stats.OldestLiveHandleKind != "request" {
		t.Fatalf("bad oldest handle: %+v", stats)
	}
}

func TestReleaseErrorIsRecorded(t *testing.T) {
	table := NewTable()
	id, err := table.Register("value", RegisterOptions{
		Runtime: "python",
		Kind:    "object",
		Release: func(value any) error {
			return errors.New("boom")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := table.Release(id); err == nil {
		t.Fatal("expected release error")
	}
	stats := table.Stats(time.Now())
	if stats.ReleaseErrors != 1 || stats.Released != 1 {
		t.Fatalf("bad stats after release error: %+v", stats)
	}
}

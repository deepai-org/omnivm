package arrow

import (
	"strings"
	"sync"
	"testing"
	"unsafe"
)

func TestNewSharedStore(t *testing.T) {
	s := NewSharedStore()
	if s == nil {
		t.Fatal("NewSharedStore returned nil")
	}
}

func TestAllocate(t *testing.T) {
	s := NewSharedStore()
	buf, err := s.Allocate("test", 1024)
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}
	if buf.Name != "test" {
		t.Fatalf("expected name 'test', got %q", buf.Name)
	}
	if buf.Len != 1024 {
		t.Fatalf("expected len 1024, got %d", buf.Len)
	}
	if len(buf.Data) != 1024 {
		t.Fatalf("expected data len 1024, got %d", len(buf.Data))
	}
}

func TestAllocateDuplicate(t *testing.T) {
	s := NewSharedStore()
	_, err := s.Allocate("test", 100)
	if err != nil {
		t.Fatalf("first Allocate failed: %v", err)
	}

	_, err = s.Allocate("test", 200)
	if err == nil {
		t.Fatal("expected error on duplicate allocation")
	}
}

func TestGet(t *testing.T) {
	s := NewSharedStore()
	s.Allocate("mydata", 512)

	buf, err := s.Get("mydata")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if buf.Len != 512 {
		t.Fatalf("expected len 512, got %d", buf.Len)
	}
}

func TestGetNotFound(t *testing.T) {
	s := NewSharedStore()
	_, err := s.Get("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent buffer")
	}
}

func TestFree(t *testing.T) {
	s := NewSharedStore()
	s.Allocate("temp", 100)

	err := s.Free("temp")
	if err != nil {
		t.Fatalf("Free failed: %v", err)
	}

	// Should be gone now
	_, err = s.Get("temp")
	if err == nil {
		t.Fatal("expected buffer to be freed")
	}
}

func TestFreeNotFound(t *testing.T) {
	s := NewSharedStore()
	err := s.Free("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent buffer")
	}
}

func TestList(t *testing.T) {
	s := NewSharedStore()
	s.Allocate("a", 10)
	s.Allocate("b", 20)
	s.Allocate("c", 30)

	names := s.List()
	if len(names) != 3 {
		t.Fatalf("expected 3 names, got %d", len(names))
	}

	nameSet := make(map[string]bool)
	for _, n := range names {
		nameSet[n] = true
	}
	for _, expected := range []string{"a", "b", "c"} {
		if !nameSet[expected] {
			t.Fatalf("expected %q in list", expected)
		}
	}
}

func TestBufferPointer(t *testing.T) {
	s := NewSharedStore()
	buf, _ := s.Allocate("ptr_test", 64)

	ptr := buf.Pointer()
	if ptr == nil {
		t.Fatal("Pointer returned nil for non-empty buffer")
	}
}

func TestBufferPointerEmpty(t *testing.T) {
	buf := &Buffer{Data: nil}
	if buf.Pointer() != nil {
		t.Fatal("expected nil pointer for empty buffer")
	}
}

func TestBufferRefCounting(t *testing.T) {
	s := NewSharedStore()
	buf, _ := s.Allocate("reftest", 100)

	buf.Retain()
	buf.Retain()
	// refs = 3 now

	r := buf.Release()
	if r != 2 {
		t.Fatalf("expected refs=2 after release, got %d", r)
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := NewSharedStore()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := string(rune('a' + i%26))
			s.Allocate(name, 100) // some will fail (duplicates), that's fine
		}(i)
	}
	wg.Wait()

	names := s.List()
	if len(names) == 0 {
		t.Fatal("expected at least some buffers")
	}
}

func TestDataReadWrite(t *testing.T) {
	s := NewSharedStore()
	buf, _ := s.Allocate("rw_test", 8)

	// Write data
	for i := 0; i < 8; i++ {
		buf.Data[i] = byte(i * 10)
	}

	// Read it back
	retrieved, _ := s.Get("rw_test")
	for i := 0; i < 8; i++ {
		if retrieved.Data[i] != byte(i*10) {
			t.Fatalf("byte %d: expected %d, got %d", i, i*10, retrieved.Data[i])
		}
	}
}

func TestSetWithDtype(t *testing.T) {
	s := NewSharedStore()
	data := []byte{1, 2, 3, 4}
	buf, err := s.SetWithDtype("typed", data, DtypeI32)
	if err != nil {
		t.Fatalf("SetWithDtype failed: %v", err)
	}
	if buf.Dtype != DtypeI32 {
		t.Fatalf("expected dtype %d, got %d", DtypeI32, buf.Dtype)
	}

	// Replace existing
	data2 := []byte{5, 6}
	buf2, err := s.SetWithDtype("typed", data2, DtypeF64)
	if err != nil {
		t.Fatalf("SetWithDtype replace failed: %v", err)
	}
	if buf2.Dtype != DtypeF64 {
		t.Fatalf("expected dtype %d after replace, got %d", DtypeF64, buf2.Dtype)
	}
	if buf2.Len != 2 {
		t.Fatalf("expected len 2 after replace, got %d", buf2.Len)
	}
}

func TestSetWithMetadataCopiesDescriptor(t *testing.T) {
	s := NewSharedStore()
	shape := []int64{2, 3}
	strides := []int64{12, 4}
	buf, err := s.SetWithMetadata("tensor", []byte{1, 2, 3, 4, 5, 6}, BufferMetadata{
		Dtype:     DtypeF32,
		Format:    "f",
		Shape:     shape,
		Strides:   strides,
		NullCount: -1,
		ReadOnly:  true,
		Ownership: "producer",
	})
	if err != nil {
		t.Fatalf("SetWithMetadata failed: %v", err)
	}
	shape[0] = 99
	strides[0] = 99

	meta := buf.Metadata()
	if meta.Dtype != DtypeF32 || meta.Format != "f" || !meta.ReadOnly || meta.Ownership != "producer" {
		t.Fatalf("bad metadata: %+v", meta)
	}
	if meta.Shape[0] != 2 || meta.Strides[0] != 12 || meta.NullCount != -1 {
		t.Fatalf("metadata was not preserved defensively: %+v", meta)
	}

	if _, err := s.SetWithMetadata("tensor", []byte{7}, BufferMetadata{Dtype: DtypeUTF8, Format: "u"}); err != nil {
		t.Fatalf("replace SetWithMetadata failed: %v", err)
	}
	meta = buf.Metadata()
	if meta.Dtype != DtypeUTF8 || meta.Format != "u" || meta.Ownership != "omnivm" {
		t.Fatalf("bad replacement metadata: %+v", meta)
	}
}

func TestBorrowZeroCopyLease(t *testing.T) {
	s := NewSharedStore()
	buf, err := s.SetWithMetadata("tensor", []byte{1, 2, 3, 4}, BufferMetadata{
		Dtype:     DtypeI32,
		Format:    "i",
		Shape:     []int64{2},
		Strides:   []int64{4},
		ReadOnly:  true,
		Ownership: "producer",
	})
	if err != nil {
		t.Fatal(err)
	}

	lease, err := s.Borrow("tensor")
	if err != nil {
		t.Fatalf("Borrow failed: %v", err)
	}
	if lease.Data != unsafe.Pointer(&buf.Data[0]) || lease.Len != 4 || lease.Dtype != DtypeI32 {
		t.Fatalf("bad lease: %+v", lease)
	}
	if lease.Metadata.Format != "i" || lease.Metadata.Shape[0] != 2 || !lease.Metadata.ReadOnly {
		t.Fatalf("bad lease metadata: %+v", lease.Metadata)
	}
	lease.Metadata.Shape[0] = 99
	if meta := buf.Metadata(); meta.Shape[0] != 2 {
		t.Fatalf("lease metadata was not defensive: %+v", meta)
	}

	buf.mu.Lock()
	refs := buf.refs
	buf.mu.Unlock()
	if refs != 2 {
		t.Fatalf("expected borrowed refs=2, got %d", refs)
	}
	stats := s.Stats()
	if stats.ActiveBorrows != 1 || stats.ActiveBorrowedBytes != 4 {
		t.Fatalf("expected active borrow count=1, got %+v", stats)
	}

	lease.Release()
	lease.Release()
	buf.mu.Lock()
	refs = buf.refs
	buf.mu.Unlock()
	if refs != 1 {
		t.Fatalf("expected released refs=1, got %d", refs)
	}

	stats = s.Stats()
	if stats.ZeroCopyBorrows != 1 || stats.ActiveBorrows != 0 || stats.ActiveBorrowedBytes != 0 || stats.Gets != 1 || stats.Releases != 1 {
		t.Fatalf("bad borrow stats: %+v", stats)
	}
}

func TestReplaceWithActiveBorrowKeepsLeaseStable(t *testing.T) {
	s := NewSharedStore()
	old, err := s.SetWithDtype("payload", []byte{1, 2, 3}, DtypeBytes)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.Borrow("payload")
	if err != nil {
		t.Fatal(err)
	}

	replacement, err := s.SetWithDtype("payload", []byte{9, 8, 7}, DtypeBytes)
	if err != nil {
		t.Fatal(err)
	}
	if replacement == old {
		t.Fatal("replacement reused a buffer with an active borrow")
	}

	borrowed := unsafe.Slice((*byte)(lease.Data), lease.Len)
	if borrowed[0] != 1 || borrowed[1] != 2 || borrowed[2] != 3 {
		t.Fatalf("borrowed view changed after replacement: %v", borrowed)
	}

	current, err := s.Get("payload")
	if err != nil {
		t.Fatal(err)
	}
	if current != replacement || current.Data[0] != 9 {
		t.Fatalf("name did not point at replacement: %+v", current)
	}
	stats := s.Stats()
	if stats.DetachedBuffers != 1 || stats.DetachedBytes != 3 || stats.ActiveBorrows != 1 || stats.ActiveBorrowedBytes != 3 {
		t.Fatalf("replacement should report detached active borrow: %+v", stats)
	}

	lease.Release()
	old.mu.Lock()
	oldRefs := old.refs
	old.mu.Unlock()
	if oldRefs != 0 {
		t.Fatalf("expected old buffer refs=0 after lease release, got %d", oldRefs)
	}
	stats = s.Stats()
	if stats.DetachedBuffers != 0 || stats.ActiveBorrows != 0 || stats.ActiveBorrowedBytes != 0 {
		t.Fatalf("detached borrow stats did not clear after release: %+v", stats)
	}
}

func TestFreeWithActiveBorrowTombstonesNameAndKeepsLeaseDiagnostics(t *testing.T) {
	s := NewSharedStore()
	buf, err := s.SetWithDtype("payload", []byte{1, 2, 3, 4}, DtypeBytes)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.Borrow("payload")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Free("payload"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("payload"); err == nil {
		t.Fatal("explicit free should tombstone the buffer name while a borrow is active")
	}
	borrowed := unsafe.Slice((*byte)(lease.Data), lease.Len)
	if borrowed[0] != 1 || borrowed[3] != 4 {
		t.Fatalf("borrowed view changed after free: %v", borrowed)
	}
	stats := s.Stats()
	if stats.LiveBuffers != 0 || stats.LiveBytes != 0 {
		t.Fatalf("freed name should not count as a live named buffer: %+v", stats)
	}
	if stats.DetachedBuffers != 1 || stats.DetachedBytes != 4 || stats.ActiveBorrows != 1 || stats.ActiveBorrowedBytes != 4 {
		t.Fatalf("active detached borrow not reported after free: %+v", stats)
	}

	lease.Release()
	buf.mu.Lock()
	refs := buf.refs
	buf.mu.Unlock()
	if refs != 0 {
		t.Fatalf("borrow release should drop detached buffer refs to zero, got %d", refs)
	}
	stats = s.Stats()
	if stats.DetachedBuffers != 0 || stats.DetachedBytes != 0 || stats.ActiveBorrows != 0 || stats.ActiveBorrowedBytes != 0 {
		t.Fatalf("detached borrow stats did not clear: %+v", stats)
	}
}

func TestBufFreeDoesNotConsumeBorrowFinalizerQueue(t *testing.T) {
	s := NewSharedStore()
	globalStore = s
	if _, err := s.SetWithDtype("payload", []byte{1, 2}, DtypeBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := s.borrowNamed("payload"); err != nil {
		t.Fatal(err)
	}

	if err := BufFree("payload"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("payload"); err == nil {
		t.Fatal("BufFree should remove the named owner reference")
	}
	stats := s.Stats()
	if stats.DetachedBuffers != 1 || stats.ActiveBorrows != 1 {
		t.Fatalf("BufFree should leave active borrow diagnostics: %+v", stats)
	}

	BufRelease("payload")
	s.DrainDeferred()
	stats = s.Stats()
	if stats.DetachedBuffers != 0 || stats.ActiveBorrows != 0 {
		t.Fatalf("borrow finalizer release did not clear detached diagnostics: %+v", stats)
	}
}

func TestBufferStatusReportsLiveReleasedAndDetachedStates(t *testing.T) {
	s := NewSharedStore()
	_, err := s.SetWithMetadata("payload", []byte{1, 2, 3, 4}, BufferMetadata{
		Dtype:     DtypeBytes,
		ReadOnly:  true,
		Ownership: "producer",
	})
	if err != nil {
		t.Fatal(err)
	}
	status := s.Status("payload")
	if !status.Live || status.State != "live" || status.Len != 4 || status.Dtype != DtypeBytes || !status.ReadOnly || status.Ownership != "producer" {
		t.Fatalf("bad live buffer status: %+v", status)
	}

	lease, err := s.borrowNamed("payload")
	if err != nil {
		t.Fatal(err)
	}
	status = s.Status("payload")
	if status.ActiveBorrows != 1 || status.ActiveBorrowedBytes != 4 {
		t.Fatalf("bad borrowed live status: %+v", status)
	}
	if err := s.Free("payload"); err != nil {
		t.Fatal(err)
	}
	status = s.Status("payload")
	if status.State != "released_detached" || !status.Released || status.Live || status.DetachedBuffers != 1 || status.DetachedBytes != 4 {
		t.Fatalf("bad released detached status: %+v", status)
	}

	lease.Release()
	status = s.Status("payload")
	if status.State != "released" || !status.Released || status.ActiveBorrows != 0 || status.DetachedBuffers != 0 {
		t.Fatalf("bad released status after borrow release: %+v", status)
	}
	if _, err := s.Get("payload"); err == nil || !strings.Contains(err.Error(), "was released") {
		t.Fatalf("released buffer get diagnostic = %v", err)
	}

	if _, err := s.SetWithDtype("payload", []byte{9}, DtypeU8); err != nil {
		t.Fatal(err)
	}
	if status = s.Status("payload"); status.State != "live" || status.Released {
		t.Fatalf("reused buffer name did not clear release status: %+v", status)
	}
}

func TestNamedBorrowReleaseTracksOriginalBufferAfterReplacement(t *testing.T) {
	s := NewSharedStore()
	old, err := s.SetWithDtype("payload", []byte{1}, DtypeBytes)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.borrowNamed("payload")
	if err != nil {
		t.Fatal(err)
	}
	if lease.Buffer != old {
		t.Fatal("named borrow did not lease original buffer")
	}

	replacement, err := s.SetWithDtype("payload", []byte{2}, DtypeBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.releaseNamedBorrow("payload"); err != nil {
		t.Fatal(err)
	}

	old.mu.Lock()
	oldRefs := old.refs
	old.mu.Unlock()
	replacement.mu.Lock()
	replacementRefs := replacement.refs
	replacement.mu.Unlock()
	if oldRefs != 0 || replacementRefs != 1 {
		t.Fatalf("release hit wrong buffer: old=%d replacement=%d", oldRefs, replacementRefs)
	}
}

func TestNamedBorrowQueueDiagnostics(t *testing.T) {
	s := NewSharedStore()
	if _, err := s.SetWithDtype("payload", []byte{1, 2, 3}, DtypeBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := s.borrowNamed("payload"); err != nil {
		t.Fatal(err)
	}

	stats := s.Stats()
	if stats.ActiveNamedBorrows != 1 || stats.NamedBorrowQueues != 1 || stats.MaxNamedBorrowQueue != 1 {
		t.Fatalf("named borrow stats = %+v, want one active queue", stats)
	}

	if err := s.releaseNamedBorrow("payload"); err != nil {
		t.Fatal(err)
	}
	stats = s.Stats()
	if stats.ActiveNamedBorrows != 0 || stats.NamedBorrowQueues != 0 || stats.MaxNamedBorrowQueue != 0 {
		t.Fatalf("named borrow stats did not clear: %+v", stats)
	}
}

func TestNamedBorrowQueueDiagnosticsExposeAmbiguousSameNameBorrows(t *testing.T) {
	s := NewSharedStore()
	if _, err := s.SetWithDtype("payload", []byte{1}, DtypeBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := s.borrowNamed("payload"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetWithDtype("payload", []byte{2}, DtypeBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := s.borrowNamed("payload"); err != nil {
		t.Fatal(err)
	}

	stats := s.Stats()
	if stats.ActiveNamedBorrows != 2 || stats.NamedBorrowQueues != 1 || stats.MaxNamedBorrowQueue != 2 {
		t.Fatalf("named borrow queue should expose same-name ambiguity: %+v", stats)
	}
	if stats.DetachedBuffers != 1 || stats.ActiveBorrows != 2 {
		t.Fatalf("replacement should leave one detached borrow and one named borrow: %+v", stats)
	}
}

func TestSharedStoreStats(t *testing.T) {
	s := NewSharedStore()
	if _, err := s.Allocate("allocated", 8); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetWithMetadata("table", []byte{1, 2, 3, 4}, BufferMetadata{Dtype: DtypeI32, Format: "i"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("table"); err != nil {
		t.Fatal(err)
	}
	s.recordBorrow()
	s.recordCopy(4)

	stats := s.Stats()
	if stats.LiveBuffers != 2 || stats.LiveBytes != 12 {
		t.Fatalf("bad live stats: %+v", stats)
	}
	if stats.Allocations != 1 || stats.Sets != 1 || stats.Gets != 1 {
		t.Fatalf("bad operation stats: %+v", stats)
	}
	if stats.CopiedBytes != 4 || stats.ZeroCopyBorrows != 1 {
		t.Fatalf("bad copy/borrow stats: %+v", stats)
	}
	if stats.BuffersByDtype["1"] != 1 || stats.BuffersByFormat["i"] != 1 {
		t.Fatalf("bad type stats: %+v", stats)
	}
	if stats.LargestBufferName != "allocated" || stats.LargestBufferSize != 8 {
		t.Fatalf("bad largest buffer stats: %+v", stats)
	}
}

func TestBufGetSet(t *testing.T) {
	// Set up global store for callback tests
	store := NewSharedStore()
	SetGlobalStore(store)
	// Reset globalOnce for this test
	globalStore = store

	// BufSet: store 1MB of data
	size := 1024 * 1024
	src := make([]byte, size)
	for i := range src {
		src[i] = byte(i % 256)
	}

	rc := BufSet("test_buf", unsafe.Pointer(&src[0]), int64(size), DtypeBytes, true)
	if rc != 0 {
		t.Fatal("BufSet failed")
	}

	// BufGet: retrieve it
	var dataOut unsafe.Pointer
	var lenOut int64
	var dtypeOut int32
	var readOnlyOut bool
	rc = BufGet("test_buf", &dataOut, &lenOut, &dtypeOut, &readOnlyOut)
	if rc != 0 {
		t.Fatal("BufGet failed")
	}
	if lenOut != int64(size) {
		t.Fatalf("expected len %d, got %d", size, lenOut)
	}
	if dtypeOut != DtypeBytes {
		t.Fatalf("expected dtype %d, got %d", DtypeBytes, dtypeOut)
	}
	if !readOnlyOut {
		t.Fatalf("expected read-only metadata from buffer bridge")
	}
	stats := store.Stats()
	if stats.CopiedBytes != int64(size) || stats.ZeroCopyBorrows != 1 || stats.Gets != 1 || stats.Sets != 1 {
		t.Fatalf("bad buffer bridge stats: %+v", stats)
	}

	// Verify data
	got := unsafe.Slice((*byte)(dataOut), lenOut)
	for i := 0; i < 100; i++ {
		if got[i] != byte(i%256) {
			t.Fatalf("byte %d: expected %d, got %d", i, i%256, got[i])
		}
	}
}

func TestBufGetNotFound(t *testing.T) {
	globalStore = NewSharedStore()
	var dataOut unsafe.Pointer
	var lenOut int64
	var dtypeOut int32
	rc := BufGet("nonexistent", &dataOut, &lenOut, &dtypeOut, nil)
	if rc != -1 {
		t.Fatal("expected -1 for nonexistent buffer")
	}
}

func TestBufRelease(t *testing.T) {
	globalStore = NewSharedStore()
	BufSet("rel_test", nil, 0, DtypeBytes, false)

	// Should not block
	BufRelease("rel_test")

	// Drain
	select {
	case name := <-DeferredRelease:
		if name != "rel_test" {
			t.Fatalf("expected rel_test, got %s", name)
		}
	default:
		t.Fatal("expected deferred release")
	}
}

func TestBufReleaseSpillsWhenDeferredChannelIsFull(t *testing.T) {
	for {
		select {
		case <-DeferredRelease:
		default:
			goto drained
		}
	}

drained:
	s := NewSharedStore()
	globalStore = s
	BufSet("overflow", nil, 0, DtypeBytes, false)

	for i := 0; i < cap(DeferredRelease); i++ {
		DeferredRelease <- "missing"
	}
	BufRelease("overflow")

	stats := s.Stats()
	if stats.DeferredDrops != 0 {
		t.Fatalf("deferred release should spill instead of dropping: %+v", stats)
	}

	s.DrainDeferred()
	if _, err := s.Get("overflow"); err == nil {
		t.Fatal("overflow release should be drained from spill queue")
	}
	stats = s.Stats()
	if stats.DeferredDrops != 0 {
		t.Fatalf("deferred release spill should not count as a drop: %+v", stats)
	}
}

func TestBufReleaseCoalescesRepeatedSpillNames(t *testing.T) {
	for {
		select {
		case <-DeferredRelease:
		default:
			goto drained
		}
	}

drained:
	s := NewSharedStore()
	globalStore = s
	BufSet("repeated", nil, 0, DtypeBytes, false)

	for i := 0; i < 10; i++ {
		if _, err := s.borrowNamed("repeated"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < cap(DeferredRelease); i++ {
		DeferredRelease <- "missing"
	}
	for i := 0; i < 10; i++ {
		BufRelease("repeated")
	}

	if deferredReleaseOverflow.total != 10 || len(deferredReleaseOverflow.order) != 1 || deferredReleaseOverflow.counts["repeated"] != 10 {
		t.Fatalf("repeated buffer releases were not coalesced: total=%d order=%v counts=%v", deferredReleaseOverflow.total, deferredReleaseOverflow.order, deferredReleaseOverflow.counts)
	}
	stats := s.Stats()
	if stats.DeferredQueueLen != cap(DeferredRelease)+10 || stats.DeferredOverflow != 1 {
		t.Fatalf("bad deferred release stats before drain: %+v", stats)
	}

	s.DrainDeferred()
	if deferredReleaseOverflow.total != 0 || len(deferredReleaseOverflow.order) != 0 {
		t.Fatalf("coalesced buffer releases were not fully drained: total=%d order=%v", deferredReleaseOverflow.total, deferredReleaseOverflow.order)
	}
	stats = s.Stats()
	if stats.DeferredQueueLen != 0 || stats.DeferredOverflow != 0 {
		t.Fatalf("bad deferred release stats after drain: %+v", stats)
	}
	buf, err := s.Get("repeated")
	if err != nil {
		t.Fatal(err)
	}
	buf.mu.Lock()
	refs := buf.refs
	buf.mu.Unlock()
	if refs != 1 {
		t.Fatalf("coalesced releases drained wrong ref count: got %d, want 1", refs)
	}
}

func TestDrainDeferred(t *testing.T) {
	s := NewSharedStore()
	globalStore = s
	BufSet("drain1", nil, 0, DtypeBytes, false)
	BufSet("drain2", nil, 0, DtypeBytes, false)

	DeferredRelease <- "drain1"
	DeferredRelease <- "drain2"

	s.DrainDeferred()

	// Both should be freed
	if _, err := s.Get("drain1"); err == nil {
		t.Fatal("drain1 should be freed")
	}
	if _, err := s.Get("drain2"); err == nil {
		t.Fatal("drain2 should be freed")
	}
}

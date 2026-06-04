package arrow

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unsafe"
)

func resetDeferredReleaseTestState() {
	for {
		select {
		case <-DeferredRelease:
		default:
			deferredReleaseOverflow.Lock()
			deferredReleaseOverflow.counts = nil
			deferredReleaseOverflow.order = nil
			deferredReleaseOverflow.total = 0
			deferredReleaseOverflow.Unlock()
			return
		}
	}
}

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
	if buf.Ownership != "omnivm" || buf.MemorySpace != "host" {
		t.Fatalf("allocated buffer ownership metadata = %q/%q, want omnivm/host", buf.Ownership, buf.MemorySpace)
	}
	meta := buf.Metadata()
	if meta.Ownership != "omnivm" || meta.MemorySpace != "host" {
		t.Fatalf("allocated buffer metadata snapshot = %q/%q, want omnivm/host", meta.Ownership, meta.MemorySpace)
	}
	lease, err := s.Borrow("test")
	if err != nil {
		t.Fatalf("borrow allocated buffer: %v", err)
	}
	defer lease.Release()
	if lease.Metadata.Ownership != "omnivm" || lease.Metadata.MemorySpace != "host" {
		t.Fatalf("allocated buffer borrow metadata = %q/%q, want omnivm/host", lease.Metadata.Ownership, lease.Metadata.MemorySpace)
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

func TestAllocateRejectsNegativeSize(t *testing.T) {
	s := NewSharedStore()
	if _, err := s.Allocate("negative", -1); err == nil {
		t.Fatal("expected error on negative allocation")
	} else if !strings.Contains(err.Error(), `buffer "negative" has negative size -1`) {
		t.Fatalf("negative allocation error = %v", err)
	}
	if status := s.Status("negative"); status.State != "missing" || status.Live {
		t.Fatalf("negative allocation registered buffer: %+v", status)
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

func TestBufferReleaseClampsAtZero(t *testing.T) {
	buf := &Buffer{refs: 1}
	if refs := buf.Release(); refs != 0 {
		t.Fatalf("first Release refs=%d, want 0", refs)
	}
	if refs := buf.Release(); refs != 0 {
		t.Fatalf("second Release refs=%d, want idempotent 0", refs)
	}
}

func TestBufferFreeAfterDirectReleaseDoesNotUnderflow(t *testing.T) {
	s := NewSharedStore()
	data := []byte{1, 2, 3}
	releases := 0
	buf, err := s.SetExternalWithMetadata("payload", unsafe.Pointer(&data[0]), int64(len(data)), BufferMetadata{
		Dtype:     DtypeBytes,
		Ownership: "producer",
	}, func() error {
		releases++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if refs := buf.Release(); refs != 0 {
		t.Fatalf("direct Release refs=%d, want 0", refs)
	}
	if err := s.Free("payload"); err != nil {
		t.Fatalf("Free after direct Release failed: %v", err)
	}
	if releases != 1 {
		t.Fatalf("producer release callback called %d times, want 1", releases)
	}
	buf.mu.Lock()
	refs := buf.refs
	buf.mu.Unlock()
	if refs != 0 {
		t.Fatalf("Free after direct Release refs=%d, want clamped 0", refs)
	}
	if status := s.Status("payload"); status.State != "released" || !status.Released {
		t.Fatalf("Free after direct Release status = %+v, want released", status)
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
		Dtype:       DtypeF32,
		Format:      "f",
		Shape:       shape,
		Strides:     strides,
		NullCount:   -1,
		ReadOnly:    true,
		Ownership:   "producer",
		MemorySpace: "host",
	})
	if err != nil {
		t.Fatalf("SetWithMetadata failed: %v", err)
	}
	shape[0] = 99
	strides[0] = 99

	meta := buf.Metadata()
	if meta.Dtype != DtypeF32 || meta.Format != "f" || !meta.ReadOnly || meta.Ownership != "producer" || meta.MemorySpace != "host" {
		t.Fatalf("bad metadata: %+v", meta)
	}
	if meta.Shape[0] != 2 || meta.Strides[0] != 12 || meta.NullCount != -1 {
		t.Fatalf("metadata was not preserved defensively: %+v", meta)
	}

	if _, err := s.SetWithMetadata("tensor", []byte{7}, BufferMetadata{Dtype: DtypeUTF8, Format: "u"}); err != nil {
		t.Fatalf("replace SetWithMetadata failed: %v", err)
	}
	meta = buf.Metadata()
	if meta.Dtype != DtypeUTF8 || meta.Format != "u" || meta.Ownership != "omnivm" || meta.MemorySpace != "host" {
		t.Fatalf("bad replacement metadata: %+v", meta)
	}
}

func TestSetWithMetadataRejectsNonHostMemorySpace(t *testing.T) {
	s := NewSharedStore()
	if _, err := s.SetWithMetadata("gpu", []byte{1}, BufferMetadata{
		Dtype:       DtypeBytes,
		MemorySpace: "cuda",
	}); err == nil || !strings.Contains(err.Error(), `memory_space "cuda" is not host-accessible`) {
		t.Fatalf("SetWithMetadata non-host memory error = %v", err)
	}
	data := []byte{1}
	released := 0
	if _, err := s.SetExternalWithMetadata("gpu-external", unsafe.Pointer(&data[0]), int64(len(data)), BufferMetadata{
		Dtype:       DtypeBytes,
		MemorySpace: "cuda",
	}, func() error {
		released++
		return nil
	}); err == nil || !strings.Contains(err.Error(), `memory_space "cuda" is not host-accessible`) {
		t.Fatalf("SetExternalWithMetadata non-host memory error = %v", err)
	}
	if released != 1 {
		t.Fatalf("rejected non-host external buffer release callback called %d times, want 1", released)
	}
	if status := s.Status("gpu"); status.State != "missing" || status.Live {
		t.Fatalf("rejected non-host buffer should not be registered: %+v", status)
	}

	releaseErr := errors.New("producer release failed")
	if _, err := s.SetExternalWithMetadata("gpu-release-error", unsafe.Pointer(&data[0]), int64(len(data)), BufferMetadata{
		Dtype:       DtypeBytes,
		MemorySpace: "cuda",
	}, func() error {
		return releaseErr
	}); !errors.Is(err, releaseErr) || !strings.Contains(err.Error(), `memory_space "cuda" is not host-accessible`) {
		t.Fatalf("SetExternalWithMetadata release failure error = %v, want non-host rejection and release failure", err)
	}
}

func TestSetWithMetadataReportsReplacementReleaseFailure(t *testing.T) {
	s := NewSharedStore()
	oldData := []byte{1, 2, 3}
	releaseErr := errors.New("old producer release failed")
	releases := 0
	if _, err := s.SetExternalWithMetadata("payload", unsafe.Pointer(&oldData[0]), int64(len(oldData)), BufferMetadata{
		Dtype:     DtypeBytes,
		Ownership: "producer",
	}, func() error {
		releases++
		return releaseErr
	}); err != nil {
		t.Fatal(err)
	}

	replacement, err := s.SetWithMetadata("payload", []byte{9, 8}, BufferMetadata{Dtype: DtypeI16})
	if !errors.Is(err, releaseErr) {
		t.Fatalf("SetWithMetadata replacement error = %v, want old release failure", err)
	}
	if replacement == nil || replacement.Len != 2 || replacement.Dtype != DtypeI16 {
		t.Fatalf("SetWithMetadata replacement = %+v, want registered owned replacement", replacement)
	}
	if releases != 1 {
		t.Fatalf("old producer release callback called %d times, want 1", releases)
	}
	status := s.Status("payload")
	if !status.Live || status.Len != 2 || status.Dtype != DtypeI16 || status.Ownership != "omnivm" {
		t.Fatalf("replacement status = %+v, want live owned replacement", status)
	}
	if stats := s.Stats(); stats.ReleaseErrors != 1 {
		t.Fatalf("replacement release stats = %+v, want one release error", stats)
	}
}

func TestSetExternalWithMetadataReportsReplacementReleaseFailure(t *testing.T) {
	s := NewSharedStore()
	oldData := []byte{1}
	newData := []byte{2}
	releaseErr := errors.New("old producer release failed")
	releases := 0
	if _, err := s.SetExternalWithMetadata("payload", unsafe.Pointer(&oldData[0]), int64(len(oldData)), BufferMetadata{
		Dtype:     DtypeBytes,
		Ownership: "producer",
	}, func() error {
		releases++
		return releaseErr
	}); err != nil {
		t.Fatal(err)
	}

	replacement, err := s.SetExternalWithMetadata("payload", unsafe.Pointer(&newData[0]), int64(len(newData)), BufferMetadata{
		Dtype:     DtypeU8,
		Ownership: "producer",
	}, nil)
	if !errors.Is(err, releaseErr) {
		t.Fatalf("SetExternalWithMetadata replacement error = %v, want old release failure", err)
	}
	if replacement == nil || replacement.Len != 1 || replacement.Dtype != DtypeU8 {
		t.Fatalf("SetExternalWithMetadata replacement = %+v, want registered external replacement", replacement)
	}
	if releases != 1 {
		t.Fatalf("old producer release callback called %d times, want 1", releases)
	}
	status := s.Status("payload")
	if !status.Live || status.Len != 1 || status.Dtype != DtypeU8 || status.Ownership != "producer" {
		t.Fatalf("replacement status = %+v, want live producer replacement", status)
	}
	if stats := s.Stats(); stats.ReleaseErrors != 1 {
		t.Fatalf("external replacement release stats = %+v, want one release error", stats)
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

func TestReplaceWithActiveBorrowKeepsOldAndNewMetadataSeparate(t *testing.T) {
	s := NewSharedStore()
	old, err := s.SetWithMetadata("payload", []byte{1, 2, 3, 4}, BufferMetadata{
		Dtype:       DtypeI32,
		Format:      "i",
		Shape:       []int64{1},
		Strides:     []int64{4},
		ReadOnly:    true,
		Ownership:   "producer",
		MemorySpace: "host",
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.Borrow("payload")
	if err != nil {
		t.Fatal(err)
	}

	replacement, err := s.SetWithMetadata("payload", []byte{9, 8}, BufferMetadata{
		Dtype:       DtypeU8,
		Format:      "C",
		Shape:       []int64{2},
		Strides:     []int64{1},
		Ownership:   "omnivm",
		MemorySpace: "host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if replacement == old {
		t.Fatal("replacement reused an actively borrowed buffer")
	}

	if lease.Metadata.Dtype != DtypeI32 || lease.Metadata.Format != "i" || !lease.Metadata.ReadOnly || lease.Metadata.Ownership != "producer" {
		t.Fatalf("borrow lease metadata changed after replacement: %+v", lease.Metadata)
	}
	oldMeta := old.Metadata()
	if oldMeta.Dtype != DtypeI32 || oldMeta.Format != "i" || !oldMeta.ReadOnly || oldMeta.Ownership != "producer" {
		t.Fatalf("old detached metadata changed after replacement: %+v", oldMeta)
	}
	current, err := s.Get("payload")
	if err != nil {
		t.Fatal(err)
	}
	currentMeta := current.Metadata()
	if current != replacement || currentMeta.Dtype != DtypeU8 || currentMeta.Format != "C" || currentMeta.ReadOnly || currentMeta.Ownership != "omnivm" {
		t.Fatalf("replacement metadata = %+v current=%p replacement=%p", currentMeta, current, replacement)
	}
	status := s.Status("payload")
	if !status.Live || status.LeaseState != "borrowed" || status.Dtype != DtypeU8 || status.Format != "C" || status.ReadOnly || status.Ownership != "omnivm" || status.DetachedBuffers != 1 || status.ActiveBorrows != 1 {
		t.Fatalf("status should report live replacement plus detached borrow: %+v", status)
	}

	lease.Release()
	status = s.Status("payload")
	if status.LeaseState != "owned" || status.DetachedBuffers != 0 || status.ActiveBorrows != 0 {
		t.Fatalf("status did not clear detached metadata after borrow release: %+v", status)
	}
}

func TestReplaceExternalWithActiveBorrowReportsDetachedLease(t *testing.T) {
	s := NewSharedStore()
	oldData := []byte{1, 2, 3}
	newData := []byte{9, 8}
	releaseErr := errors.New("old producer release failed")
	releases := 0
	old, err := s.SetExternalWithMetadata("payload", unsafe.Pointer(&oldData[0]), int64(len(oldData)), BufferMetadata{
		Dtype:     DtypeBytes,
		Ownership: "producer",
	}, func() error {
		releases++
		return releaseErr
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.Borrow("payload")
	if err != nil {
		t.Fatal(err)
	}

	replacement, err := s.SetExternalWithMetadata("payload", unsafe.Pointer(&newData[0]), int64(len(newData)), BufferMetadata{
		Dtype:     DtypeU8,
		Ownership: "producer",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if replacement == old {
		t.Fatal("external replacement reused a buffer with an active borrow")
	}
	if releases != 0 {
		t.Fatalf("external replacement called old release callback before borrow release: %d", releases)
	}
	if lease.Data != unsafe.Pointer(&oldData[0]) || lease.Len != int64(len(oldData)) {
		t.Fatalf("borrowed external lease changed after replacement: data=%v len=%d", lease.Data, lease.Len)
	}

	stats := s.Stats()
	if stats.DetachedBuffers != 1 || stats.DetachedBytes != 3 || stats.ActiveBorrows != 1 || stats.ActiveBorrowedBytes != 3 {
		t.Fatalf("external replacement should report detached active borrow: %+v", stats)
	}
	status := s.Status("payload")
	if !status.Live || status.LeaseState != "borrowed" || status.DetachedBuffers != 1 || status.DetachedBytes != 3 || status.ActiveBorrows != 1 || status.ActiveBorrowedBytes != 3 || status.Len != 2 || status.Dtype != DtypeU8 || status.Ownership != "producer" {
		t.Fatalf("external replacement status hid detached borrow: %+v", status)
	}

	if err := lease.ReleaseWithError(); !errors.Is(err, releaseErr) {
		t.Fatalf("ReleaseWithError after external replacement = %v, want producer release failure", err)
	}
	if releases != 1 {
		t.Fatalf("external old release callback called %d times, want 1", releases)
	}
	if err := lease.ReleaseWithError(); !errors.Is(err, releaseErr) {
		t.Fatalf("second ReleaseWithError after external replacement = %v, want cached producer release failure", err)
	}
	stats = s.Stats()
	if stats.DetachedBuffers != 0 || stats.ActiveBorrows != 0 || stats.ActiveBorrowedBytes != 0 {
		t.Fatalf("external detached borrow stats did not clear after release: %+v", stats)
	}
	if stats.ReleaseErrors != 1 {
		t.Fatalf("external detached borrow release errors = %+v, want one release error", stats)
	}
}

func TestBufferStatusReportsDetachedBorrowAfterNameReuse(t *testing.T) {
	s := NewSharedStore()
	if _, err := s.SetWithDtype("payload", []byte{1, 2, 3}, DtypeBytes); err != nil {
		t.Fatal(err)
	}
	lease, err := s.Borrow("payload")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetWithDtype("payload", []byte{9}, DtypeU8); err != nil {
		t.Fatal(err)
	}

	status := s.Status("payload")
	if !status.Live || status.Released || status.State != "live" || status.LeaseState != "borrowed" {
		t.Fatalf("status should report live replacement with outstanding borrow: %+v", status)
	}
	if status.Len != 1 || status.Dtype != DtypeU8 || status.DetachedBuffers != 1 || status.DetachedBytes != 3 || status.ActiveBorrows != 1 || status.ActiveBorrowedBytes != 3 {
		t.Fatalf("status hid same-name detached borrow after replacement: %+v", status)
	}

	lease.Release()
	status = s.Status("payload")
	if status.State != "live" || status.LeaseState != "owned" || status.DetachedBuffers != 0 || status.ActiveBorrows != 0 {
		t.Fatalf("status did not clear detached borrow after release: %+v", status)
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

func TestBorrowedBufferReleaseWithErrorReportsLastOwnerReleaseFailure(t *testing.T) {
	s := NewSharedStore()
	data := []byte{1, 2, 3, 4}
	releaseErr := errors.New("producer release failed")
	releases := 0
	buf, err := s.SetExternalWithMetadata("payload", unsafe.Pointer(&data[0]), int64(len(data)), BufferMetadata{
		Dtype:     DtypeBytes,
		Format:    "C",
		Ownership: "producer",
	}, func() error {
		releases++
		return releaseErr
	})
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
	if releases != 0 {
		t.Fatalf("free with active borrow called release callback early: %d", releases)
	}
	if err := lease.ReleaseWithError(); !errors.Is(err, releaseErr) {
		t.Fatalf("ReleaseWithError = %v, want release callback failure", err)
	}
	if releases != 1 {
		t.Fatalf("release callback called %d times, want 1", releases)
	}
	if err := lease.ReleaseWithError(); !errors.Is(err, releaseErr) {
		t.Fatalf("second ReleaseWithError = %v, want release callback failure", err)
	}
	buf.mu.Lock()
	refs := buf.refs
	buf.mu.Unlock()
	if refs != 0 {
		t.Fatalf("released external buffer refs = %d, want 0", refs)
	}
	status := s.Status("payload")
	if status.State != "released" || status.Len != 4 || status.Dtype != DtypeBytes || status.Format != "C" || status.Ownership != "producer" {
		t.Fatalf("released external buffer status lost metadata: %+v", status)
	}
	if status.ReleaseError != "producer release failed" {
		t.Fatalf("released external buffer status release error = %q, want producer release failed", status.ReleaseError)
	}
	if stats := s.Stats(); stats.ReleaseErrors != 1 {
		t.Fatalf("released external buffer stats = %+v, want one release error", stats)
	}
}

func TestBufferFreeRecordsProducerReleaseFailureInStatus(t *testing.T) {
	s := NewSharedStore()
	data := []byte{1, 2, 3, 4}
	releaseErr := errors.New("producer release failed")
	if _, err := s.SetExternalWithMetadata("payload", unsafe.Pointer(&data[0]), int64(len(data)), BufferMetadata{
		Dtype:       DtypeBytes,
		Format:      "C",
		Ownership:   "producer",
		MemorySpace: "host",
	}, func() error {
		return releaseErr
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Free("payload"); !errors.Is(err, releaseErr) {
		t.Fatalf("Free = %v, want producer release failure", err)
	}
	status := s.Status("payload")
	if status.State != "released" || status.LeaseState != "released" || status.Len != 4 || status.Dtype != DtypeBytes || status.Format != "C" || status.Ownership != "producer" || status.MemorySpace != "host" {
		t.Fatalf("released external buffer status lost metadata after free failure: %+v", status)
	}
	if status.ReleaseError != "producer release failed" {
		t.Fatalf("release error status = %q, want producer release failed", status.ReleaseError)
	}
	if stats := s.Stats(); stats.ReleaseErrors != 1 {
		t.Fatalf("free release failure stats = %+v, want one release error", stats)
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

func TestDeferredBorrowReleaseRecordsProducerReleaseFailure(t *testing.T) {
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
	data := []byte{1, 2, 3}
	releaseErr := errors.New("producer release failed")
	if _, err := s.SetExternalWithMetadata("payload", unsafe.Pointer(&data[0]), int64(len(data)), BufferMetadata{
		Dtype:     DtypeBytes,
		Ownership: "producer",
	}, func() error {
		return releaseErr
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.borrowNamed("payload"); err != nil {
		t.Fatal(err)
	}
	if err := s.Free("payload"); err != nil {
		t.Fatal(err)
	}

	BufRelease("payload")
	s.DrainDeferred()
	stats := s.Stats()
	if stats.LiveBuffers != 0 || stats.DetachedBuffers != 0 || stats.ActiveBorrows != 0 || stats.ReleaseErrors != 1 {
		t.Fatalf("deferred release failure stats = %+v, want quiet release error recorded and no live borrow", stats)
	}
	status := s.Status("payload")
	if status.State != "released" || status.ReleaseError != "producer release failed" {
		t.Fatalf("deferred release failure status = %+v, want released tombstone with release error", status)
	}
}

func TestBufferStatusReportsLiveReleasedAndDetachedStates(t *testing.T) {
	s := NewSharedStore()
	_, err := s.SetWithMetadata("payload", []byte{1, 2, 3, 4}, BufferMetadata{
		Dtype:     DtypeBytes,
		Format:    "C",
		ReadOnly:  true,
		Ownership: "producer",
	})
	if err != nil {
		t.Fatal(err)
	}
	status := s.Status("payload")
	if !status.Live || status.State != "live" || status.LeaseState != "owned" || status.Len != 4 || status.Dtype != DtypeBytes || status.Format != "C" || !status.ReadOnly || status.Ownership != "producer" || status.MemorySpace != "host" {
		t.Fatalf("bad live buffer status: %+v", status)
	}

	lease, err := s.borrowNamed("payload")
	if err != nil {
		t.Fatal(err)
	}
	status = s.Status("payload")
	if status.LeaseState != "borrowed" || status.ActiveBorrows != 1 || status.ActiveBorrowedBytes != 4 || status.ActiveNamedBorrows != 1 || status.NamedBorrowQueue != 1 {
		t.Fatalf("bad borrowed live status: %+v", status)
	}
	if err := s.Free("payload"); err != nil {
		t.Fatal(err)
	}
	status = s.Status("payload")
	if status.State != "released_detached" || status.LeaseState != "detached" || !status.Released || status.Live || status.DetachedBuffers != 1 || status.DetachedBytes != 4 || status.ActiveNamedBorrows != 1 || status.NamedBorrowQueue != 1 || status.Len != 4 || status.Dtype != DtypeBytes || status.Format != "C" || !status.ReadOnly || status.Ownership != "producer" || status.MemorySpace != "host" {
		t.Fatalf("bad released detached status: %+v", status)
	}

	lease.Release()
	status = s.Status("payload")
	if status.State != "released" || status.LeaseState != "released" || !status.Released || status.ActiveBorrows != 0 || status.ActiveNamedBorrows != 0 || status.NamedBorrowQueue != 0 || status.DetachedBuffers != 0 || status.Len != 4 || status.Dtype != DtypeBytes || status.Format != "C" || !status.ReadOnly || status.Ownership != "producer" || status.MemorySpace != "host" {
		t.Fatalf("bad released status after borrow release: %+v", status)
	}
	if _, err := s.Get("payload"); err == nil || !strings.Contains(err.Error(), "was released") {
		t.Fatalf("released buffer get diagnostic = %v", err)
	}

	if _, err := s.SetWithDtype("payload", []byte{9}, DtypeU8); err != nil {
		t.Fatal(err)
	}
	if status = s.Status("payload"); status.State != "live" || status.LeaseState != "owned" || status.Released {
		t.Fatalf("reused buffer name did not clear release status: %+v", status)
	}
	if status = s.Status("missing"); status.State != "missing" || status.LeaseState != "missing" || status.Live || status.Released {
		t.Fatalf("bad missing buffer status: %+v", status)
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

func TestNamedBorrowDirectReleaseClearsReleaseQueue(t *testing.T) {
	s := NewSharedStore()
	if _, err := s.SetWithDtype("payload", []byte{1, 2, 3}, DtypeBytes); err != nil {
		t.Fatal(err)
	}
	lease, err := s.borrowNamed("payload")
	if err != nil {
		t.Fatal(err)
	}

	lease.Release()
	stats := s.Stats()
	if stats.ActiveBorrows != 0 || stats.ActiveNamedBorrows != 0 || stats.NamedBorrowQueues != 0 {
		t.Fatalf("direct named lease release left stale diagnostics: %+v", stats)
	}
	if err := s.releaseNamedBorrow("payload"); err == nil {
		t.Fatal("stale named release queue allowed a second borrow release")
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
	status := s.Status("payload")
	if status.ActiveNamedBorrows != 2 || status.NamedBorrowQueue != 2 || status.DetachedBuffers != 1 || status.ActiveBorrows != 2 {
		t.Fatalf("buffer status should expose same-name named borrow ambiguity: %+v", status)
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
	data := []byte{1}
	dataOut := unsafe.Pointer(&data[0])
	lenOut := int64(99)
	dtypeOut := int32(DtypeF64)
	readOnlyOut := true
	rc := BufGet("nonexistent", &dataOut, &lenOut, &dtypeOut, &readOnlyOut)
	if rc != -1 {
		t.Fatal("expected -1 for nonexistent buffer")
	}
	if dataOut != nil || lenOut != 0 || dtypeOut != 0 || readOnlyOut {
		t.Fatalf("failed BufGet left stale outputs: data=%v len=%d dtype=%d readOnly=%v", dataOut, lenOut, dtypeOut, readOnlyOut)
	}
}

func TestBufGetReleasedNameClearsOutputs(t *testing.T) {
	store := NewSharedStore()
	globalStore = store
	data := []byte{1, 2, 3}
	if _, err := store.SetWithDtype("released", data, DtypeBytes); err != nil {
		t.Fatal(err)
	}
	if err := store.Free("released"); err != nil {
		t.Fatal(err)
	}

	dataOut := unsafe.Pointer(&data[0])
	lenOut := int64(len(data))
	dtypeOut := int32(DtypeBytes)
	readOnlyOut := true
	rc := BufGet("released", &dataOut, &lenOut, &dtypeOut, &readOnlyOut)
	if rc != -1 {
		t.Fatalf("BufGet released name rc=%d, want -1", rc)
	}
	if dataOut != nil || lenOut != 0 || dtypeOut != 0 || readOnlyOut {
		t.Fatalf("released BufGet left stale outputs: data=%v len=%d dtype=%d readOnly=%v", dataOut, lenOut, dtypeOut, readOnlyOut)
	}
}

func TestBufGetRejectsNilRequiredOutputs(t *testing.T) {
	globalStore = NewSharedStore()
	if rc := BufGet("missing", nil, nil, nil, nil); rc != -1 {
		t.Fatalf("BufGet nil outputs rc=%d, want -1", rc)
	}
}

func TestBufSetRejectsInvalidNativeInputs(t *testing.T) {
	store := NewSharedStore()
	globalStore = store
	if rc := BufSet("negative", nil, -1, DtypeBytes, false); rc != -1 {
		t.Fatalf("BufSet negative length rc=%d, want -1", rc)
	}
	if rc := BufSet("nil-data", nil, 4, DtypeBytes, false); rc != -1 {
		t.Fatalf("BufSet nil non-empty data rc=%d, want -1", rc)
	}
	if status := store.Status("negative"); status.State != "missing" {
		t.Fatalf("invalid BufSet registered negative buffer: %+v", status)
	}
	if status := store.Status("nil-data"); status.State != "missing" {
		t.Fatalf("invalid BufSet registered nil-data buffer: %+v", status)
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
	if _, err := s.Get("overflow"); err != nil {
		t.Fatalf("stale overflow cleanup release should not free owner: %v", err)
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

func TestBufReleaseBoundsDistinctSpillNamesAndReportsDrops(t *testing.T) {
	resetDeferredReleaseTestState()
	defer resetDeferredReleaseTestState()

	s := NewSharedStore()
	globalStore = s
	for i := 0; i < cap(DeferredRelease); i++ {
		DeferredRelease <- "missing"
	}
	for i := 0; i < maxDeferredReleaseOverflowNames; i++ {
		BufRelease("spill-" + strconv.Itoa(i))
	}

	BufRelease("dropped")
	BufRelease("spill-0")

	stats := s.Stats()
	if stats.DeferredDrops != 1 {
		t.Fatalf("deferred release overflow drops = %d, want 1", stats.DeferredDrops)
	}
	if stats.DeferredOverflow != maxDeferredReleaseOverflowNames {
		t.Fatalf("deferred release overflow names = %d, want %d", stats.DeferredOverflow, maxDeferredReleaseOverflowNames)
	}
	if stats.DeferredQueueLen != cap(DeferredRelease)+maxDeferredReleaseOverflowNames+1 {
		t.Fatalf("deferred release queue len = %d, want channel + bounded spill + repeated release", stats.DeferredQueueLen)
	}
	if _, ok := deferredReleaseOverflow.counts["dropped"]; ok {
		t.Fatal("dropped overflow name was retained")
	}
	if got := deferredReleaseOverflow.counts["spill-0"]; got != 2 {
		t.Fatalf("repeated overflow name count = %d, want 2", got)
	}
}

func TestDrainDeferred(t *testing.T) {
	s := NewSharedStore()
	globalStore = s
	BufSet("drain1", nil, 0, DtypeBytes, false)
	BufSet("drain2", nil, 0, DtypeBytes, false)
	if _, err := s.borrowNamed("drain1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.borrowNamed("drain2"); err != nil {
		t.Fatal(err)
	}

	DeferredRelease <- "drain1"
	DeferredRelease <- "drain2"

	s.DrainDeferred()

	for _, name := range []string{"drain1", "drain2"} {
		buf, err := s.Get(name)
		if err != nil {
			t.Fatalf("%s owner should remain live after borrow cleanup: %v", name, err)
		}
		buf.mu.Lock()
		refs := buf.refs
		buf.mu.Unlock()
		if refs != 1 {
			t.Fatalf("%s refs after deferred borrow cleanup = %d, want owner ref only", name, refs)
		}
	}
	stats := s.Stats()
	if stats.ActiveNamedBorrows != 0 || stats.NamedBorrowQueues != 0 || stats.ActiveBorrows != 0 {
		t.Fatalf("deferred borrow cleanup left stale diagnostics: %+v", stats)
	}
}

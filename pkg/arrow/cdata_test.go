package arrow

import (
	"testing"
	"unsafe"
)

func TestBorrowCArrowArrayExportsPrimitiveView(t *testing.T) {
	s := NewSharedStore()
	buf, err := s.SetWithMetadata("numbers", []byte{1, 0, 0, 0, 2, 0, 0, 0}, BufferMetadata{
		Dtype:     DtypeI32,
		Format:    "i",
		NullCount: 0,
		ReadOnly:  true,
	})
	if err != nil {
		t.Fatal(err)
	}

	view, err := s.BorrowCArrowArray("numbers")
	if err != nil {
		t.Fatalf("BorrowCArrowArray failed: %v", err)
	}
	if view.Format() != "i" {
		t.Fatalf("format = %q, want i", view.Format())
	}
	if view.Length() != 2 {
		t.Fatalf("length = %d, want 2", view.Length())
	}
	if view.BufferPointer(0) != nil {
		t.Fatal("null bitmap buffer should be nil")
	}
	if view.BufferPointer(1) != unsafe.Pointer(&buf.Data[0]) {
		t.Fatal("value buffer is not the borrowed backing memory")
	}

	buf.mu.Lock()
	refs := buf.refs
	buf.mu.Unlock()
	if refs != 2 {
		t.Fatalf("refs after Arrow borrow = %d, want 2", refs)
	}

	view.Release()
	view.Release()
	buf.mu.Lock()
	refs = buf.refs
	buf.mu.Unlock()
	if refs != 1 {
		t.Fatalf("refs after Arrow release = %d, want 1", refs)
	}

	stats := s.Stats()
	if stats.ZeroCopyBorrows != 1 || stats.Releases != 1 {
		t.Fatalf("bad Arrow borrow stats: %+v", stats)
	}
}

func TestBorrowCArrowArrayExportsSignedInt8View(t *testing.T) {
	s := NewSharedStore()
	buf, err := s.SetWithMetadata("signed", []byte{0xff, 0x00, 0x02}, BufferMetadata{
		Dtype:     DtypeI8,
		Format:    "c",
		NullCount: 0,
		ReadOnly:  true,
	})
	if err != nil {
		t.Fatal(err)
	}

	view, err := s.BorrowCArrowArray("signed")
	if err != nil {
		t.Fatalf("BorrowCArrowArray failed: %v", err)
	}
	defer view.Release()
	if view.Format() != "c" {
		t.Fatalf("format = %q, want c", view.Format())
	}
	if view.Length() != 3 {
		t.Fatalf("length = %d, want 3", view.Length())
	}
	if view.BufferPointer(1) != unsafe.Pointer(&buf.Data[0]) {
		t.Fatal("signed int8 value buffer is not the borrowed backing memory")
	}
}

func TestBorrowCArrowArrayExportsEmptyPrimitiveView(t *testing.T) {
	s := NewSharedStore()
	if _, err := s.SetWithMetadata("empty", nil, BufferMetadata{
		Dtype:     DtypeI32,
		Format:    "i",
		Shape:     []int64{0},
		NullCount: 0,
		ReadOnly:  true,
	}); err != nil {
		t.Fatal(err)
	}

	view, err := s.BorrowCArrowArray("empty")
	if err != nil {
		t.Fatalf("BorrowCArrowArray failed: %v", err)
	}
	defer view.Release()
	if view.Format() != "i" {
		t.Fatalf("format = %q, want i", view.Format())
	}
	if view.Length() != 0 {
		t.Fatalf("length = %d, want 0", view.Length())
	}
	if view.BufferPointer(1) != nil {
		t.Fatal("empty value buffer should be nil")
	}
}

func TestBorrowCArrowArrayExportsContiguousOffsetView(t *testing.T) {
	s := NewSharedStore()
	buf, err := s.SetWithMetadata("window", []byte{
		9, 0, 0, 0,
		1, 0, 0, 0,
		2, 0, 0, 0,
		8, 0, 0, 0,
	}, BufferMetadata{
		Dtype:     DtypeI32,
		Format:    "i",
		Shape:     []int64{2},
		Offset:    4,
		NullCount: 0,
		ReadOnly:  true,
	})
	if err != nil {
		t.Fatal(err)
	}

	view, err := s.BorrowCArrowArray("window")
	if err != nil {
		t.Fatalf("BorrowCArrowArray failed: %v", err)
	}
	defer view.Release()
	if view.Length() != 2 {
		t.Fatalf("length = %d, want 2", view.Length())
	}
	if view.Array.offset != 1 {
		t.Fatalf("Arrow offset = %d, want 1", view.Array.offset)
	}
	if view.BufferPointer(1) != unsafe.Pointer(&buf.Data[0]) {
		t.Fatal("offset Arrow export should keep the original value buffer pointer")
	}
}

func TestBorrowCArrowArrayRejectsMisalignedPrimitive(t *testing.T) {
	s := NewSharedStore()
	if _, err := s.SetWithDtype("bad", []byte{1, 2, 3}, DtypeI32); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BorrowCArrowArray("bad"); err == nil {
		t.Fatal("expected misaligned primitive export to fail")
	}

	stats := s.Stats()
	if stats.ZeroCopyBorrows != 1 || stats.Releases != 1 {
		t.Fatalf("failed Arrow export did not release borrow: %+v", stats)
	}
}

func TestBorrowCArrowArrayRejectsNonContiguousStrides(t *testing.T) {
	s := NewSharedStore()
	if _, err := s.SetWithMetadata("strided", []byte{2, 1, 0, 0, 4, 3, 0, 0, 6, 5}, BufferMetadata{
		Dtype:   DtypeU16,
		Format:  "S",
		Shape:   []int64{3},
		Strides: []int64{4},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BorrowCArrowArray("strided"); err == nil {
		t.Fatal("expected non-contiguous strided primitive export to fail")
	}
}

func TestBorrowCArrowArrayRejectsNonPrimitiveFormat(t *testing.T) {
	s := NewSharedStore()
	if _, err := s.SetWithMetadata("text", []byte("hello"), BufferMetadata{Dtype: DtypeUTF8, Format: "u"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BorrowCArrowArray("text"); err == nil {
		t.Fatal("expected variable-width Arrow format export to fail")
	}
}

func TestImportCArrowArrayWithOffsetPreservesLogicalWindow(t *testing.T) {
	source := NewSharedStore()
	src, err := source.SetWithMetadata("window", []byte{
		9, 0, 0, 0,
		1, 0, 0, 0,
		2, 0, 0, 0,
		8, 0, 0, 0,
	}, BufferMetadata{
		Dtype:  DtypeI32,
		Format: "i",
		Shape:  []int64{2},
		Offset: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := source.BorrowCArrowArray("window")
	if err != nil {
		t.Fatalf("BorrowCArrowArray failed: %v", err)
	}
	defer view.Release()

	target := NewSharedStore()
	if err := target.ImportCArrowArray("imported-window", unsafe.Pointer(view.Schema), unsafe.Pointer(view.Array)); err != nil {
		t.Fatalf("ImportCArrowArray failed: %v", err)
	}
	imported, err := target.Get("imported-window")
	if err != nil {
		t.Fatalf("Get imported failed: %v", err)
	}
	if imported.Pointer() != unsafe.Pointer(&src.Data[4]) {
		t.Fatal("imported Arrow offset window did not preserve the logical value buffer pointer")
	}
	if imported.Len != 8 {
		t.Fatalf("imported length = %d, want 8", imported.Len)
	}
	if err := target.Free("imported-window"); err != nil {
		t.Fatalf("Free imported failed: %v", err)
	}
}

func TestImportCArrowArrayTakesOwnedPrimitiveDataPlaneZeroCopy(t *testing.T) {
	source := NewSharedStore()
	src, err := source.SetWithMetadata("numbers", []byte{1, 0, 0, 0, 2, 0, 0, 0}, BufferMetadata{
		Dtype:  DtypeI32,
		Format: "i",
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := source.BorrowCArrowArray("numbers")
	if err != nil {
		t.Fatalf("BorrowCArrowArray failed: %v", err)
	}
	defer view.Release()

	target := NewSharedStore()
	if err := target.ImportCArrowArray("imported", unsafe.Pointer(view.Schema), unsafe.Pointer(view.Array)); err != nil {
		t.Fatalf("ImportCArrowArray failed: %v", err)
	}

	imported, err := target.Get("imported")
	if err != nil {
		t.Fatalf("Get imported failed: %v", err)
	}
	if imported.Pointer() != unsafe.Pointer(&src.Data[0]) {
		t.Fatal("imported Arrow buffer did not preserve the producer value buffer pointer")
	}
	if len(imported.Data) != 0 || imported.ExternalData == nil {
		t.Fatalf("imported buffer should be external zero-copy memory, got data=%d external=%v", len(imported.Data), imported.ExternalData)
	}
	if imported.Dtype != DtypeI32 || imported.Format != "i" {
		t.Fatalf("imported metadata dtype=%d format=%q, want i32/i", imported.Dtype, imported.Format)
	}

	src.mu.Lock()
	refs := src.refs
	src.mu.Unlock()
	if refs != 2 {
		t.Fatalf("source Arrow descriptor was released too early: refs=%d, want 2", refs)
	}
	if err := target.Free("imported"); err != nil {
		t.Fatalf("Free imported failed: %v", err)
	}
	src.mu.Lock()
	refs = src.refs
	src.mu.Unlock()
	if refs != 1 {
		t.Fatalf("source Arrow descriptor was not released with imported buffer: refs=%d, want 1", refs)
	}
	stats := target.Stats()
	if stats.CopiedBytes != 0 || stats.ZeroCopyImports != 1 || stats.Sets != 1 || stats.Releases != 1 {
		t.Fatalf("bad import stats: %+v", stats)
	}
}

func TestImportCArrowArrayPreservesSignedInt8Format(t *testing.T) {
	source := NewSharedStore()
	src, err := source.SetWithMetadata("signed", []byte{0xff, 0x00, 0x02}, BufferMetadata{
		Dtype:  DtypeI8,
		Format: "c",
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := source.BorrowCArrowArray("signed")
	if err != nil {
		t.Fatalf("BorrowCArrowArray failed: %v", err)
	}
	defer view.Release()

	target := NewSharedStore()
	if err := target.ImportCArrowArray("imported-signed", unsafe.Pointer(view.Schema), unsafe.Pointer(view.Array)); err != nil {
		t.Fatalf("ImportCArrowArray failed: %v", err)
	}
	imported, err := target.Get("imported-signed")
	if err != nil {
		t.Fatalf("Get imported failed: %v", err)
	}
	if imported.Pointer() != unsafe.Pointer(&src.Data[0]) {
		t.Fatal("imported signed int8 buffer did not preserve the producer value buffer pointer")
	}
	if imported.Dtype != DtypeI8 || imported.Format != "c" {
		t.Fatalf("imported metadata dtype=%d format=%q, want i8/c", imported.Dtype, imported.Format)
	}
	if err := target.Free("imported-signed"); err != nil {
		t.Fatalf("Free imported failed: %v", err)
	}
}

func TestImportCArrowArrayPreservesNullablePrimitiveZeroCopy(t *testing.T) {
	source := NewSharedStore()
	src, err := source.SetWithMetadata("numbers", []byte{1, 0, 0, 0, 2, 0, 0, 0, 3, 0, 0, 0}, BufferMetadata{
		Dtype:  DtypeI32,
		Format: "i",
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := source.BorrowCArrowArray("numbers")
	if err != nil {
		t.Fatalf("BorrowCArrowArray failed: %v", err)
	}
	defer view.Release()
	validity := []byte{0b00000101}
	view.Array.null_count = 1
	view.setBufferPointer(0, unsafe.Pointer(&validity[0]))

	target := NewSharedStore()
	if err := target.ImportCArrowArray("nullable", unsafe.Pointer(view.Schema), unsafe.Pointer(view.Array)); err != nil {
		t.Fatalf("ImportCArrowArray nullable failed: %v", err)
	}
	imported, err := target.Get("nullable")
	if err != nil {
		t.Fatalf("Get nullable failed: %v", err)
	}
	if imported.Pointer() != unsafe.Pointer(&src.Data[0]) {
		t.Fatal("nullable imported Arrow buffer did not preserve the producer value buffer pointer")
	}
	if imported.NullCount != 1 || imported.ValidityLen != 1 || imported.ExternalValidity != unsafe.Pointer(&validity[0]) {
		t.Fatalf("nullable metadata = nulls %d validity_len %d validity %v", imported.NullCount, imported.ValidityLen, imported.ExternalValidity)
	}
	lease, err := target.Borrow("nullable")
	if err != nil {
		t.Fatalf("Borrow nullable failed: %v", err)
	}
	if lease.Validity != unsafe.Pointer(&validity[0]) || lease.Metadata.ValidityBytes != 1 {
		t.Fatalf("nullable borrow validity = %v/%d metadata=%+v", lease.Validity, lease.ValidityLen, lease.Metadata)
	}
	lease.Release()
	if err := target.Free("nullable"); err != nil {
		t.Fatalf("Free nullable failed: %v", err)
	}
	stats := target.Stats()
	if stats.CopiedBytes != 0 || stats.ZeroCopyImports != 1 {
		t.Fatalf("nullable import should stay zero-copy: %+v", stats)
	}
}

func TestImportCArrowArrayRejectsNullablePrimitiveWithoutValidityBitmap(t *testing.T) {
	source := NewSharedStore()
	if _, err := source.SetWithMetadata("numbers", []byte{1, 0, 0, 0}, BufferMetadata{
		Dtype:  DtypeI32,
		Format: "i",
	}); err != nil {
		t.Fatal(err)
	}
	view, err := source.BorrowCArrowArray("numbers")
	if err != nil {
		t.Fatalf("BorrowCArrowArray failed: %v", err)
	}
	defer view.Release()
	view.Array.null_count = 1

	target := NewSharedStore()
	if err := target.ImportCArrowArray("bad", unsafe.Pointer(view.Schema), unsafe.Pointer(view.Array)); err == nil {
		t.Fatal("expected nullable primitive without validity bitmap to fail")
	}
}

func TestImportCArrowArrayRejectsOversizedTransferWithoutLeakingBorrow(t *testing.T) {
	source := NewSharedStore()
	src, err := source.SetWithMetadata("numbers", []byte{1, 0, 0, 0}, BufferMetadata{
		Dtype:  DtypeI32,
		Format: "i",
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := source.BorrowCArrowArray("numbers")
	if err != nil {
		t.Fatalf("BorrowCArrowArray failed: %v", err)
	}
	defer view.Release()

	const hugeArrowLength = (1<<63-1)/4 + 1
	view.Array.length = hugeArrowLength

	target := NewSharedStore()
	if err := target.ImportCArrowArray("oversized", unsafe.Pointer(view.Schema), unsafe.Pointer(view.Array)); err == nil {
		t.Fatal("expected oversized Arrow import to fail")
	}
	src.mu.Lock()
	refs := src.refs
	src.mu.Unlock()
	if refs != 1 {
		t.Fatalf("failed oversized import leaked source borrow refs = %d, want 1", refs)
	}
	if stats := target.Stats(); stats.LiveBuffers != 0 || stats.ZeroCopyImports != 0 {
		t.Fatalf("failed oversized import should not register a target buffer: %+v", stats)
	}
}

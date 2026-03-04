package arrow

import (
	"sync"
	"testing"
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

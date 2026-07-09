package mounter

import (
	"sync"
	"testing"
)

func TestUidAllocator_Allocate(t *testing.T) {
	a := NewUidAllocator(nil)

	uid, err := a.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if uid != uidMin {
		t.Fatalf("expected %d, got %d", uidMin, uid)
	}

	uid2, err := a.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if uid2 != uidMin+1 {
		t.Fatalf("expected %d, got %d", uidMin+1, uid2)
	}
}

func TestUidAllocator_Release(t *testing.T) {
	a := NewUidAllocator(nil)

	uid, _ := a.Allocate()
	a.Release(uid)

	// After release, the cursor has advanced, so next allocation gives uidMin+1
	uid2, err := a.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if uid2 != uidMin+1 {
		t.Fatalf("expected %d, got %d", uidMin+1, uid2)
	}
}

func TestUidAllocator_WrapAround(t *testing.T) {
	a := NewUidAllocator(nil)
	a.nextUid = uidMax

	uid, err := a.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if uid != uidMax {
		t.Fatalf("expected %d, got %d", uidMax, uid)
	}

	uid2, err := a.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if uid2 != uidMin {
		t.Fatalf("expected wraparound to %d, got %d", uidMin, uid2)
	}
}

func TestUidAllocator_Exhausted(t *testing.T) {
	// Pre-allocate all UIDs
	all := make([]uint32, 0, uidMax-uidMin+1)
	for uid := uidMin; uid <= uidMax; uid++ {
		all = append(all, uid)
	}
	a := NewUidAllocator(all)

	_, err := a.Allocate()
	if err == nil {
		t.Fatal("expected error when pool is exhausted")
	}
}

func TestUidAllocator_PreAllocated(t *testing.T) {
	a := NewUidAllocator([]uint32{uidMin, uidMin + 1})

	uid, err := a.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if uid != uidMin+2 {
		t.Fatalf("expected %d (skipping pre-allocated), got %d", uidMin+2, uid)
	}
}

func TestUidAllocator_ReleaseThenReuse(t *testing.T) {
	// Allocate a few, release the first, verify it can be reused after wrap
	a := NewUidAllocator(nil)

	uid1, _ := a.Allocate() // uidMin
	a.Allocate()            // uidMin+1
	a.Allocate()            // uidMin+2

	a.Release(uid1)

	// Position cursor at uidMin (simulating a full wrap)
	a.nextUid = uidMin

	uid, err := a.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if uid != uidMin {
		t.Fatalf("expected reuse of released %d, got %d", uidMin, uid)
	}
}

func TestUidAllocator_Concurrent(t *testing.T) {
	a := NewUidAllocator(nil)

	const goroutines = 100
	results := make([]uint32, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			uid, err := a.Allocate()
			if err != nil {
				t.Error(err)
				return
			}
			results[idx] = uid
		}(i)
	}
	wg.Wait()

	// All UIDs should be unique
	seen := make(map[uint32]struct{})
	for _, uid := range results {
		if _, dup := seen[uid]; dup {
			t.Fatalf("duplicate UID allocated: %d", uid)
		}
		seen[uid] = struct{}{}
	}
}

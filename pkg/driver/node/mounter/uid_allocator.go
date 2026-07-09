package mounter

import (
	"fmt"
	"sync"
)

const (
	uidMin = uint32(2000)
	uidMax = uint32(65534)
)

// UidAllocatorInterface is the interface for UID allocation.
type UidAllocatorInterface interface {
	Allocate() (uint32, error)
	Release(uid uint32)
}

// UidAllocator manages a pool of UIDs for Mountpoint child process isolation.
type UidAllocator struct {
	mu      sync.Mutex
	nextUid uint32
	inUse   map[uint32]struct{}
}

// NewUidAllocator creates a UidAllocator with the given UIDs already reserved (e.g. restored from disk on startup).
func NewUidAllocator(allocated []uint32) *UidAllocator {
	inUse := make(map[uint32]struct{}, len(allocated))
	for _, uid := range allocated {
		inUse[uid] = struct{}{}
	}
	return &UidAllocator{
		nextUid: uidMin,
		inUse:   inUse,
	}
}

// Allocate returns the next available UID from the pool.
func (a *UidAllocator) Allocate() (uint32, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	poolSize := uidMax - uidMin + 1
	for range poolSize {
		uid := a.nextUid
		a.nextUid++
		if a.nextUid > uidMax {
			a.nextUid = uidMin
		}
		if _, taken := a.inUse[uid]; !taken {
			a.inUse[uid] = struct{}{}
			return uid, nil
		}
	}
	return 0, fmt.Errorf("uid pool exhausted: all %d UIDs in range [%d, %d] are in use", poolSize, uidMin, uidMax)
}

// Release returns a UID to the pool.
func (a *UidAllocator) Release(uid uint32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.inUse, uid)
}

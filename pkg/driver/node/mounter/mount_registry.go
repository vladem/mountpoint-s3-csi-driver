package mounter

import (
	"sync"
	"time"
)

// MountRecord tracks per-mount metadata (UID for credential provisioning) and records the
// fact of resource allocation. An entry is only removed after all associated resources
// (credentials, UID) are cleaned up.
type MountRecord struct {
	Uid       uint32
	CommDir   string // comm directory at mount time; used for cleanup even if commDir changes
	Target    string // mount target path; used to detect stale entries
	CreatedAt time.Time
}

// MountRegistry tracks active mounts. It serves two purposes:
//   - Provides stable mount metadata (e.g. UID) across repeated Mount calls for the same volume.
//   - Acts as a resource ledger: an entry's existence means resources are allocated and
//     must be cleaned up before the entry can be removed.
//
// TODO: Persist to disk, cache in RAM, recover on restart. Entry in the registry is immutable after creation.
// TODO: Couple record creation with successful mount; on new mount request, perform full cleanup cycle
// and re-create the record rather than reusing a potentially stale one.
type MountRegistry struct {
	mu     sync.Mutex
	mounts map[string]*MountRecord
}

func NewMountRegistry() *MountRegistry {
	return &MountRegistry{
		mounts: make(map[string]*MountRecord),
	}
}

// Create adds a new mount record. Returns false if already exists.
func (r *MountRegistry) Create(mountId string, record *MountRecord) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.mounts[mountId]; exists {
		return false
	}
	r.mounts[mountId] = record
	return true
}

// Get returns the record for a mount, or nil if not found.
func (r *MountRegistry) Get(mountId string) *MountRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mounts[mountId]
}

// Delete removes a mount record.
func (r *MountRegistry) Delete(mountId string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.mounts, mountId)
}

const staleRecordAge = 10 * time.Minute

// StaleMounts returns mount IDs of records older than staleRecordAge whose target
// is no longer a mount point. The isMountPoint function is called without the lock held.
func (r *MountRegistry) StaleMounts(isMountPoint func(target string) (bool, error)) []string {
	r.mu.Lock()
	candidates := make(map[string]*MountRecord)
	now := time.Now()
	for id, rec := range r.mounts {
		if now.Sub(rec.CreatedAt) > staleRecordAge {
			candidates[id] = rec
		}
	}
	r.mu.Unlock()

	var stale []string
	for id, rec := range candidates {
		mounted, err := isMountPoint(rec.Target)
		if err != nil || !mounted {
			stale = append(stale, id)
		}
	}
	return stale
}

package x11

import "sync"

// SeqMap tracks the mapping from client-assigned X11 sequence numbers to
// the originating app's sequence numbers, for reply rewriting on reconnect.
type SeqMap struct {
	mu      sync.Mutex
	pending map[uint32]seqEntry
}

type seqEntry struct {
	appSeq  uint32
	discard bool // true for synthesis requests (no corresponding app request)
}

// Record maps clientSeq → appSeq. Use discard=true for synthesis requests
// that generate replies but have no corresponding app request.
func (m *SeqMap) Record(clientSeq, appSeq uint32, discard bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil {
		m.pending = make(map[uint32]seqEntry)
	}
	m.pending[clientSeq] = seqEntry{appSeq: appSeq, discard: discard}
}

// Resolve looks up clientSeq and returns (appSeq, discard, ok).
// If ok is false the sequence number was not recorded (pass-through).
func (m *SeqMap) Resolve(clientSeq uint32) (appSeq uint32, discard bool, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.pending[clientSeq]
	if ok {
		delete(m.pending, clientSeq)
	}
	return e.appSeq, e.discard, ok
}

// AtomMap tracks the mapping from original session atom IDs to new session atom IDs.
type AtomMap struct {
	mu sync.RWMutex
	m  map[uint32]uint32
}

// Set records that original atom ID orig maps to newID in the reconnected session.
func (a *AtomMap) Set(orig, newID uint32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.m == nil {
		a.m = make(map[uint32]uint32)
	}
	a.m[orig] = newID
}

// Get returns the new atom ID for the original atom ID.
func (a *AtomMap) Get(orig uint32) (uint32, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	v, ok := a.m[orig]
	return v, ok
}

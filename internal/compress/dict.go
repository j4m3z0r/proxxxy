package compress

import (
	"container/list"
	"sync"

	"github.com/cespare/xxhash/v2"
)

// Action describes what the caller should send for a given command sequence.
type Action int

const (
	ActionPassthrough Action = iota // send raw bytes (not in dict yet)
	ActionDefine                    // send DICT_DEFINE(id, bytes) then add to clientDict
	ActionRef                       // send DICT_REF(id) — client already has it
)

type dictEntry struct {
	id       uint64
	fp       uint64
	data     []byte
	inClient bool
	elem     *list.Element // position in LRU list
}

// Dict is the server-side command sequence dictionary.
// It is authoritative: only the server decides when to add or evict entries.
type Dict struct {
	mu         sync.Mutex
	capacity   int // max total bytes across all entries
	used       int
	nextID     uint64
	byFP       map[uint64]*dictEntry // fingerprint → entry
	lru        *list.List            // LRU order, front = most recently used
	expiredIDs []uint64              // IDs evicted since last DrainExpired call
}

// NewDict creates a Dict with the given capacity in bytes.
func NewDict(capacityBytes int) *Dict {
	return &Dict{
		capacity: capacityBytes,
		byFP:     make(map[uint64]*dictEntry),
		lru:      list.New(),
	}
}

// Classify determines what to send for seq.
// Returns (action, id, data):
//   - ActionDefine: send DICT_DEFINE(id, data), entry is now in client dict
//   - ActionRef:    send DICT_REF(id), client already has it
func (d *Dict) Classify(seq []byte) (Action, uint64, []byte) {
	fp := xxhash.Sum64(seq)

	d.mu.Lock()
	defer d.mu.Unlock()

	if e, ok := d.byFP[fp]; ok {
		d.lru.MoveToFront(e.elem)
		if e.inClient {
			return ActionRef, e.id, nil
		}
		e.inClient = true
		return ActionDefine, e.id, e.data
	}

	needed := len(seq)
	if needed > d.capacity {
		return ActionPassthrough, 0, nil
	}
	for d.used+needed > d.capacity && d.lru.Len() > 0 {
		d.evictLRU()
	}

	d.nextID++
	id := d.nextID
	cp := make([]byte, len(seq))
	copy(cp, seq)
	e := &dictEntry{id: id, fp: fp, data: cp, inClient: true}
	e.elem = d.lru.PushFront(e)
	d.byFP[fp] = e
	d.used += len(seq)
	return ActionDefine, id, cp
}

func (d *Dict) evictLRU() {
	back := d.lru.Back()
	if back == nil {
		return
	}
	e := back.Value.(*dictEntry)
	d.lru.Remove(back)
	delete(d.byFP, e.fp)
	d.used -= len(e.data)
	if e.inClient {
		d.expiredIDs = append(d.expiredIDs, e.id)
	}
}

// DrainExpired returns and clears the list of IDs evicted since the last call.
// The server must send DICT_EXPIRE for each.
func (d *Dict) DrainExpired() []uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := d.expiredIDs
	d.expiredIDs = nil
	return ids
}

// ClientDisconnected marks all entries as not-in-client so the next connection
// gets ActionDefine for each entry it needs.
func (d *Dict) ClientDisconnected() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range d.byFP {
		e.inClient = false
	}
}

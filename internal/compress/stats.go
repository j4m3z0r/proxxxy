package compress

import "sync/atomic"

// Stats holds per-connection compression counters. Safe for concurrent use.
type Stats struct {
	BytesIn      atomic.Int64
	BytesOut     atomic.Int64
	DictHits     atomic.Int64
	DictDefines  atomic.Int64
	DictPasses   atomic.Int64
	TemplateHits atomic.Int64
	TemplateDefs atomic.Int64
}

// Snapshot is a point-in-time copy of Stats.
type Snapshot struct {
	BytesIn      int64
	BytesOut     int64
	DictHits     int64
	DictDefines  int64
	DictPasses   int64
	TemplateHits int64
	TemplateDefs int64
}

// Snapshot returns a consistent point-in-time copy of all counters.
func (s *Stats) Snapshot() Snapshot {
	return Snapshot{
		BytesIn:      s.BytesIn.Load(),
		BytesOut:     s.BytesOut.Load(),
		DictHits:     s.DictHits.Load(),
		DictDefines:  s.DictDefines.Load(),
		DictPasses:   s.DictPasses.Load(),
		TemplateHits: s.TemplateHits.Load(),
		TemplateDefs: s.TemplateDefs.Load(),
	}
}

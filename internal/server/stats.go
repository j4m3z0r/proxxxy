package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	"james.id.au/proxxxy/internal/compress"
)

type connStatsJSON struct {
	BytesIn      int64   `json:"bytes_in"`
	BytesOut     int64   `json:"bytes_out"`
	Ratio        float64 `json:"ratio"`
	DictHits     int64   `json:"dict_hits"`
	DictDefines  int64   `json:"dict_defines"`
	DictPasses   int64   `json:"dict_passthroughs"`
	TemplateHits int64   `json:"template_hits"`
	TemplateDefs int64   `json:"template_defines"`
}

type statsResponse struct {
	ClientConnected   bool                     `json:"client_connected"`
	ActiveConnections int                      `json:"active_connections"`
	Aggregate         connStatsJSON            `json:"aggregate"`
	Connections       map[string]connStatsJSON `json:"connections,omitempty"`
}

func snapshotToConn(snap compress.Snapshot) connStatsJSON {
	ratio := 1.0
	if snap.BytesIn > 0 {
		ratio = float64(snap.BytesOut) / float64(snap.BytesIn)
	}
	return connStatsJSON{
		BytesIn:      snap.BytesIn,
		BytesOut:     snap.BytesOut,
		Ratio:        ratio,
		DictHits:     snap.DictHits,
		DictDefines:  snap.DictDefines,
		DictPasses:   snap.DictPasses,
		TemplateHits: snap.TemplateHits,
		TemplateDefs: snap.TemplateDefs,
	}
}

func sumConn(a, b connStatsJSON) connStatsJSON {
	return connStatsJSON{
		BytesIn:      a.BytesIn + b.BytesIn,
		BytesOut:     a.BytesOut + b.BytesOut,
		DictHits:     a.DictHits + b.DictHits,
		DictDefines:  a.DictDefines + b.DictDefines,
		DictPasses:   a.DictPasses + b.DictPasses,
		TemplateHits: a.TemplateHits + b.TemplateHits,
		TemplateDefs: a.TemplateDefs + b.TemplateDefs,
	}
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	aggregateOnly := r.URL.Query().Get("aggregate") == "1"

	s.mu.Lock()
	clientConnected := s.clientConn != nil
	s.mu.Unlock()

	var agg connStatsJSON
	var count int
	conns := make(map[string]connStatsJSON)

	s.encoders.Range(func(key, val any) bool {
		connID := key.(uint32)
		enc := val.(*compress.Encoder)
		cs := snapshotToConn(enc.Stats.Snapshot())
		agg = sumConn(agg, cs)
		count++
		if !aggregateOnly {
			conns[strconv.FormatUint(uint64(connID), 10)] = cs
		}
		return true
	})

	if agg.BytesIn > 0 {
		agg.Ratio = float64(agg.BytesOut) / float64(agg.BytesIn)
	} else {
		agg.Ratio = 1.0
	}

	resp := statsResponse{
		ClientConnected:   clientConnected,
		ActiveConnections: count,
		Aggregate:         agg,
	}
	if !aggregateOnly {
		resp.Connections = conns
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp) //nolint:errcheck
}

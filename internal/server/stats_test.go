package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"james.id.au/proxxxy/internal/compress"
)

func TestHandleStats_Full(t *testing.T) {
	s := &Server{}
	enc := compress.NewEncoder(1, 4*1024*1024)
	enc.Stats.BytesIn.Add(1000)
	enc.Stats.BytesOut.Add(800)
	enc.Stats.DictHits.Add(5)
	s.encoders.Store(uint32(1), enc)

	req := httptest.NewRequest("GET", "/stats", nil)
	w := httptest.NewRecorder()
	s.handleStats(w, req)

	var resp statsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.ActiveConnections != 1 {
		t.Errorf("ActiveConnections: got %d want 1", resp.ActiveConnections)
	}
	if resp.Aggregate.BytesIn != 1000 {
		t.Errorf("Aggregate.BytesIn: got %d want 1000", resp.Aggregate.BytesIn)
	}
	if want := 800.0 / 1000.0; resp.Aggregate.Ratio != want {
		t.Errorf("Aggregate.Ratio: got %f want %f", resp.Aggregate.Ratio, want)
	}
	if resp.Connections == nil {
		t.Error("Connections should be present in full response")
	}
	if _, ok := resp.Connections["1"]; !ok {
		t.Error("Connections should have key '1'")
	}
}

func TestHandleStats_AggregateOnly(t *testing.T) {
	s := &Server{}
	enc := compress.NewEncoder(1, 4*1024*1024)
	enc.Stats.BytesIn.Add(500)
	enc.Stats.BytesOut.Add(500)
	s.encoders.Store(uint32(1), enc)

	req := httptest.NewRequest("GET", "/stats?aggregate=1", nil)
	w := httptest.NewRecorder()
	s.handleStats(w, req)

	var resp statsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Connections != nil {
		t.Errorf("Connections should be absent in aggregate response, got %v", resp.Connections)
	}
	if resp.Aggregate.Ratio != 1.0 {
		t.Errorf("Ratio: got %f want 1.0 (no compression while bypassed)", resp.Aggregate.Ratio)
	}
}

func TestHandleStats_Empty(t *testing.T) {
	s := &Server{}

	req := httptest.NewRequest("GET", "/stats", nil)
	w := httptest.NewRecorder()
	s.handleStats(w, req)

	var resp statsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.ActiveConnections != 0 {
		t.Errorf("ActiveConnections: got %d want 0", resp.ActiveConnections)
	}
	if resp.Aggregate.Ratio != 1.0 {
		t.Errorf("Ratio should be 1.0 when no data, got %f", resp.Aggregate.Ratio)
	}
}

package compress

import "testing"

func TestStatsSnapshot(t *testing.T) {
	s := &Stats{}
	s.BytesIn.Add(1000)
	s.BytesOut.Add(800)
	s.DictHits.Add(5)
	s.DictDefines.Add(3)
	s.DictPasses.Add(1)
	s.TemplateHits.Add(7)
	s.TemplateDefs.Add(2)

	snap := s.Snapshot()

	checks := []struct {
		name string
		got  int64
		want int64
	}{
		{"BytesIn", snap.BytesIn, 1000},
		{"BytesOut", snap.BytesOut, 800},
		{"DictHits", snap.DictHits, 5},
		{"DictDefines", snap.DictDefines, 3},
		{"DictPasses", snap.DictPasses, 1},
		{"TemplateHits", snap.TemplateHits, 7},
		{"TemplateDefs", snap.TemplateDefs, 2},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %d want %d", c.name, c.got, c.want)
		}
	}
}

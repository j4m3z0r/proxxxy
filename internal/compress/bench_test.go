package compress_test

import (
	"testing"

	"james.id.au/proxxxy/internal/compress"
)

// BenchmarkDictHitRate simulates 1000 PolyFillRectangle commands cycling
// through 2 colour values (like testclient) and measures dict performance.
func BenchmarkDictHitRate(b *testing.B) {
	d := compress.NewDict(64 * 1024 * 1024)
	makeCmd := func(color byte) []byte {
		return []byte{70, 0, 4, 0, 1, 0, 0x10, 0, 1, 0, 0x20, 0,
			50, 0, 50, 0, 200, 0, 200, 0, color, 0, 0, 0}
	}
	cmds := [2][]byte{makeCmd(0xFF), makeCmd(0x00)}

	defines, refs := 0, 0
	for i := 0; i < b.N; i++ {
		action, _, _ := d.Classify(cmds[i%2])
		if action == compress.ActionDefine {
			defines++
		} else if action == compress.ActionRef {
			refs++
		}
	}
	b.ReportMetric(float64(refs)/float64(defines+refs)*100, "%hit")
}

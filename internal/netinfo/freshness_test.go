package netinfo

import (
	"testing"

	"github.com/pingular/pingularity/internal/stats"
)

// THE CONNECTION PANEL'S FRESHNESS IS ALERTABLE. A published snapshot whose
// lookups succeeded stamps netinfo.last_ok_ts (a gauge under pingularity_stat);
// a failed refresh must NOT stamp it - the whole point is that a box whose
// lookups keep failing shows a timestamp that stops advancing.
func TestMarkFreshStampsOnlySuccessfulSnapshots(t *testing.T) {
	stats.ResetForTest()
	markFresh(Info{Error: "ip lookup failed"})
	if _, ok := stats.Lifetime().Gauges["netinfo.last_ok_ts"]; ok {
		t.Error("a failed lookup stamped the freshness gauge - staleness would be undetectable")
	}
	markFresh(Info{PublicIP: "203.0.113.7", ISP: "AS64500 Example"})
	if v := stats.Lifetime().Gauges["netinfo.last_ok_ts"]; v == 0 {
		t.Error("a successful snapshot did not stamp the freshness gauge")
	}
}

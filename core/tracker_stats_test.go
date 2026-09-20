package core

import "testing"

func TestStatsSnapshotConsumePreservesConcurrentTraffic(t *testing.T) {
	s := NewStatsTracker()
	read, write := s.getReadCounters("in", "out", "alice")
	for _, c := range read {
		c.Add(100)
	}
	for _, c := range write {
		c.Add(40)
	}
	snap := s.SnapshotStats()
	if len(*snap) != 6 {
		t.Fatalf("expected one row per resource and direction, got %d", len(*snap))
	}
	// Traffic arriving while the database transaction is in flight belongs to
	// the next batch and must survive consumption of the old snapshot.
	for _, c := range read {
		c.Add(7)
	}
	for _, c := range write {
		c.Add(3)
	}
	s.ConsumeStats(*snap)
	got := s.SnapshotStats()
	for _, st := range *got {
		want := int64(7)
		if !st.Direction {
			want = 3
		}
		if st.Traffic != want {
			t.Fatalf("remaining traffic = %d, want %d for %+v", st.Traffic, want, st)
		}
	}
}

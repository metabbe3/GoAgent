package logskill

import (
	"testing"
	"time"
)

func TestCorrelationDetection(t *testing.T) {
	baseTime, _ := time.Parse("2006-01-02 15:04:05", "2026-11-09 12:00:00")

	// Synthesize log lines where ERROR always appears 1 second after WARN
	var entries []TimedPattern

	for i := 0; i < 5; i++ {
		warnTime := baseTime.Add(time.Duration(i*10) * time.Second)
		errTime := warnTime.Add(1 * time.Second)

		entries = append(entries, TimedPattern{
			Timestamp: warnTime,
			Pattern:   "[WARN] High memory threshold reached",
		})
		entries = append(entries, TimedPattern{
			Timestamp: errTime,
			Pattern:   "[ERROR] Process killed by OOM killer",
		})
	}

	corrs := DetectCorrelations(entries)
	if len(corrs) == 0 {
		t.Fatalf("expected at least 1 correlation pair, got 0")
	}

	foundPair := false
	for _, c := range corrs {
		if c.A == "[WARN] High memory threshold reached" && c.B == "[ERROR] Process killed by OOM killer" {
			foundPair = true
			if c.Count < 3 {
				t.Errorf("expected count >= 3, got %d", c.Count)
			}
			if c.AvgGapMS != 1000.0 {
				t.Errorf("expected avg gap 1000ms, got %f", c.AvgGapMS)
			}
			if c.Note != "co-occurrence, not proven causation" {
				t.Errorf("unexpected note: %s", c.Note)
			}
		}
	}

	if !foundPair {
		t.Errorf("expected WARN -> ERROR correlation not found in: %+v", corrs)
	}
}

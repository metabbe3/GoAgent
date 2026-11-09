package logskill

import (
	"fmt"
	"testing"
	"time"
)

func TestBurstDetection(t *testing.T) {
	// Synthesize a 60-minute log dataset where minute 42 has a huge spike
	baseTime, err := time.Parse("2006-01-02 15:04:05", "2026-11-09 10:00:00")
	if err != nil {
		t.Fatalf("time parse error: %v", err)
	}

	var entries []TimedPattern

	// Minutes 0 to 59: normal traffic (1-2 entries per minute)
	for minute := 0; minute < 60; minute++ {
		normalCount := 1
		if minute == 42 {
			// Minute 42 has a spike: 50 requests with pattern "POST /api/checkout {N}"
			normalCount = 50
		}
		for i := 0; i < normalCount; i++ {
			ts := baseTime.Add(time.Duration(minute)*time.Minute + time.Duration(i)*time.Second)
			entries = append(entries, TimedPattern{
				Timestamp: ts,
				Pattern:   "POST /api/checkout {N}",
			})
		}
	}

	bursts := DetectBursts(entries)
	if len(bursts) == 0 {
		t.Fatalf("expected at least 1 burst detected at minute 42, got 0")
	}

	// Verify that the burst is at minute 42
	foundMin42 := false
	for _, b := range bursts {
		if b.Pattern == "POST /api/checkout {N}" {
			expectedStart := fmt.Sprintf("2026-11-09 10:42:00")
			if b.WindowStart == expectedStart {
				foundMin42 = true
				if b.Count != 50 {
					t.Errorf("expected burst count 50, got %d", b.Count)
				}
				if b.ZScore <= 3.0 {
					t.Errorf("expected z_score > 3.0, got %f", b.ZScore)
				}
			}
		}
	}

	if !foundMin42 {
		t.Errorf("burst at minute 42 not detected in bursts: %+v", bursts)
	}
}

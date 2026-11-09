package logskill

import (
	"path/filepath"
	"testing"
	"time"
)

func TestPatternStoreMergeAndNewFlag(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "patterns", "test_app.log.json")

	now1 := time.Date(2024, 3, 29, 10, 0, 0, 0, time.UTC)

	// First batch: Pattern A (5) and Pattern B (10)
	batch1 := map[string]*PatternData{
		"{DATE} [INFO] Pattern A": {Count: 5, Sample: "2024-03-29 [INFO] Pattern A", Level: "INFO"},
		"{DATE} [WARN] Pattern B": {Count: 10, Sample: "2024-03-29 [WARN] Pattern B", Level: "WARN"},
	}

	newFlags1, err := UpdateLearnedPatterns(storePath, batch1, now1)
	if err != nil {
		t.Fatalf("UpdateLearnedPatterns batch 1 failed: %v", err)
	}

	// In first run, both should be flagged as new
	if !newFlags1["{DATE} [INFO] Pattern A"] {
		t.Errorf("expected Pattern A to be new in batch 1")
	}
	if !newFlags1["{DATE} [WARN] Pattern B"] {
		t.Errorf("expected Pattern B to be new in batch 1")
	}

	// Verify persistence
	store1, err := LoadStore(storePath)
	if err != nil {
		t.Fatalf("LoadStore failed: %v", err)
	}
	if store1["{DATE} [INFO] Pattern A"].Count != 5 {
		t.Errorf("expected count 5, got %d", store1["{DATE} [INFO] Pattern A"].Count)
	}

	// Second batch: Pattern A (3 more), Pattern C (new, 2)
	now2 := time.Date(2024, 3, 29, 11, 0, 0, 0, time.UTC)
	batch2 := map[string]*PatternData{
		"{DATE} [INFO] Pattern A":  {Count: 3, Sample: "2024-03-29 [INFO] Pattern A updated", Level: "INFO"},
		"{DATE} [ERROR] Pattern C": {Count: 2, Sample: "2024-03-29 [ERROR] Pattern C", Level: "ERROR"},
	}

	newFlags2, err := UpdateLearnedPatterns(storePath, batch2, now2)
	if err != nil {
		t.Fatalf("UpdateLearnedPatterns batch 2 failed: %v", err)
	}

	// Pattern A was already seen, so new should be false
	if newFlags2["{DATE} [INFO] Pattern A"] {
		t.Errorf("expected Pattern A to NOT be new in batch 2")
	}
	// Pattern C is first seen, so new should be true
	if !newFlags2["{DATE} [ERROR] Pattern C"] {
		t.Errorf("expected Pattern C to be new in batch 2")
	}

	// Verify merged count in store: Pattern A count should be 5 + 3 = 8
	store2, err := LoadStore(storePath)
	if err != nil {
		t.Fatalf("LoadStore failed: %v", err)
	}

	recA := store2["{DATE} [INFO] Pattern A"]
	if recA.Count != 8 {
		t.Errorf("expected Pattern A count to be 8, got %d", recA.Count)
	}
	if !recA.FirstSeen.Equal(now1) {
		t.Errorf("expected FirstSeen %v, got %v", now1, recA.FirstSeen)
	}
	if !recA.LastSeen.Equal(now2) {
		t.Errorf("expected LastSeen %v, got %v", now2, recA.LastSeen)
	}

	// Test ListLearnedPatterns
	summaries, err := ListLearnedPatterns(filepath.Dir(storePath), "test_app.log")
	if err != nil {
		t.Fatalf("ListLearnedPatterns failed: %v", err)
	}
	if len(summaries) != 3 {
		t.Errorf("expected 3 learned patterns in summary, got %d", len(summaries))
	}
}

func TestDailyBucketAndBaselineSpikeDrop(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "patterns", "server.log.json")

	day1 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	day3 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)

	patSpike := "connection reset by peer"
	patDrop := "cache lookup successful"

	// Day 1: count 10 for each
	_, _ = UpdateLearnedPatterns(storePath, map[string]*PatternData{
		patSpike: {Count: 10, Sample: patSpike, Level: "ERROR"},
		patDrop:  {Count: 10, Sample: patDrop, Level: "INFO"},
	}, day1)

	// Day 2: count 10 for each (avg across day 1 and 2 is 10)
	_, _ = UpdateLearnedPatterns(storePath, map[string]*PatternData{
		patSpike: {Count: 10, Sample: patSpike, Level: "ERROR"},
		patDrop:  {Count: 10, Sample: patDrop, Level: "INFO"},
	}, day2)

	// Verify daily map is populated in store
	st, err := LoadStore(storePath)
	if err != nil {
		t.Fatalf("failed to load store: %v", err)
	}
	if st[patSpike].Daily["2026-11-01"] != 10 || st[patSpike].Daily["2026-11-02"] != 10 {
		t.Errorf("daily map not correctly populated: %+v", st[patSpike].Daily)
	}

	// Day 3: current run has patSpike count=30 (>2x 10) and patDrop count=2 (<0.5x 10)
	currentRun := map[string]*PatternData{
		patSpike: {Count: 30, Sample: patSpike, Level: "ERROR"},
		patDrop:  {Count: 2, Sample: patDrop, Level: "INFO"},
	}

	changes, err := CompareWithBaseline(storePath, currentRun, day3.Format("2006-01-02"))
	if err != nil {
		t.Fatalf("CompareWithBaseline failed: %v", err)
	}

	foundSpike := false
	foundDrop := false

	for _, c := range changes {
		if c.Pattern == patSpike {
			if !c.Spike {
				t.Errorf("expected spike:true for %s, got %+v", patSpike, c)
			}
			if c.HistoricalAvg != 10.0 {
				t.Errorf("expected historical avg 10.0, got %f", c.HistoricalAvg)
			}
			foundSpike = true
		}
		if c.Pattern == patDrop {
			if !c.Drop {
				t.Errorf("expected drop:true for %s, got %+v", patDrop, c)
			}
			if c.HistoricalAvg != 10.0 {
				t.Errorf("expected historical avg 10.0, got %f", c.HistoricalAvg)
			}
			foundDrop = true
		}
	}

	if !foundSpike {
		t.Errorf("expected spike change stat not found")
	}
	if !foundDrop {
		t.Errorf("expected drop change stat not found")
	}
}

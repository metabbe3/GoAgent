package logskill

import (
	"math"
	"sort"
	"time"
)

// CorrelationStat represents a detected co-occurrence pattern between two events.
type CorrelationStat struct {
	A        string  `json:"a"`
	B        string  `json:"b"`
	Count    int64   `json:"count"`
	AvgGapMS float64 `json:"avg_gap_ms"`
	Note     string  `json:"note"`
}

type pairAccumulator struct {
	count      int64
	totalGapMS int64
}

// DetectCorrelations identifies recurring pairs of patterns appearing within ±2 seconds
// on consecutive lines. Requires parsed timestamps.
func DetectCorrelations(entries []TimedPattern) []CorrelationStat {
	if len(entries) < 2 {
		return nil
	}

	pairMap := make(map[string]*pairAccumulator)
	pairKeys := make(map[string][2]string)

	for i := 0; i < len(entries)-1; i++ {
		curr := entries[i]
		next := entries[i+1]

		if curr.Timestamp.IsZero() || next.Timestamp.IsZero() {
			continue
		}

		if curr.Pattern == "" || next.Pattern == "" || curr.Pattern == next.Pattern {
			continue
		}

		gap := next.Timestamp.Sub(curr.Timestamp)
		if gap < 0 {
			gap = -gap
		}

		if gap <= 2*time.Second {
			gapMS := gap.Milliseconds()
			key := curr.Pattern + " -> " + next.Pattern

			if acc, exists := pairMap[key]; exists {
				acc.count++
				acc.totalGapMS += gapMS
			} else {
				pairMap[key] = &pairAccumulator{
					count:      1,
					totalGapMS: gapMS,
				}
				pairKeys[key] = [2]string{curr.Pattern, next.Pattern}
			}
		}
	}

	var results []CorrelationStat
	for key, acc := range pairMap {
		if acc.count >= 3 {
			p := pairKeys[key]
			avgGap := float64(acc.totalGapMS) / float64(acc.count)
			results = append(results, CorrelationStat{
				A:        p[0],
				B:        p[1],
				Count:    acc.count,
				AvgGapMS: math.Round(avgGap*100) / 100,
				Note:     "co-occurrence, not proven causation",
			})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Count != results[j].Count {
			return results[i].Count > results[j].Count
		}
		return results[i].AvgGapMS < results[j].AvgGapMS
	})

	if len(results) > 10 {
		results = results[:10]
	}

	return results
}

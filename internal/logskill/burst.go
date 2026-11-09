package logskill

import (
	"math"
	"sort"
	"time"
)

// BurstStat represents a detected frequency spike in a specific time window.
type BurstStat struct {
	Pattern     string  `json:"pattern"`
	WindowStart string  `json:"window_start"`
	Count       int64   `json:"count"`
	Avg         float64 `json:"avg"`
	ZScore      float64 `json:"z_score"`
}

// TimedPattern tracks a pattern occurrence at a specific timestamp.
type TimedPattern struct {
	Timestamp time.Time
	Pattern   string
}

// DetectBursts calculates baseline frequency distributions across time windows
// and identifies burst events where count > mean + 3*stdev AND count >= 5.
func DetectBursts(entries []TimedPattern) []BurstStat {
	if len(entries) < 5 {
		return nil
	}

	// 1. Determine min and max timestamp
	var minTime, maxTime time.Time
	first := true
	for _, e := range entries {
		if e.Timestamp.IsZero() {
			continue
		}
		if first {
			minTime = e.Timestamp
			maxTime = e.Timestamp
			first = false
		} else {
			if e.Timestamp.Before(minTime) {
				minTime = e.Timestamp
			}
			if e.Timestamp.After(maxTime) {
				maxTime = e.Timestamp
			}
		}
	}

	if first || minTime.Equal(maxTime) {
		return nil
	}

	// 2. Select window size: 1 hour if span > 24h, else 1 minute
	windowDuration := time.Minute
	if maxTime.Sub(minTime) > 24*time.Hour {
		windowDuration = time.Hour
	}

	// 3. Bucket counts per pattern and per window
	// pattern -> window_time -> count
	patternWindows := make(map[string]map[time.Time]int64)
	patternTotals := make(map[string]int64)

	for _, e := range entries {
		if e.Timestamp.IsZero() {
			continue
		}
		wStart := e.Timestamp.Truncate(windowDuration)
		if patternWindows[e.Pattern] == nil {
			patternWindows[e.Pattern] = make(map[time.Time]int64)
		}
		patternWindows[e.Pattern][wStart]++
		patternTotals[e.Pattern]++
	}

	// Total continuous windows from minTime to maxTime
	startBucket := minTime.Truncate(windowDuration)
	endBucket := maxTime.Truncate(windowDuration)
	numWindows := int(endBucket.Sub(startBucket)/windowDuration) + 1
	if numWindows < 2 {
		numWindows = 2
	}

	var allBursts []BurstStat

	// 4. Compute mean, stdev, and burst status per pattern
	for pat, windows := range patternWindows {
		totalCount := patternTotals[pat]
		mean := float64(totalCount) / float64(numWindows)

		// Calculate variance across all windows (including zero-count windows)
		var varianceSum float64
		nonZeroWindows := len(windows)
		zeroWindows := numWindows - nonZeroWindows
		if zeroWindows < 0 {
			zeroWindows = 0
		}

		for _, count := range windows {
			diff := float64(count) - mean
			varianceSum += diff * diff
		}
		// Zero-count windows contributed (0 - mean)^2
		varianceSum += float64(zeroWindows) * (mean * mean)

		variance := varianceSum / float64(numWindows)
		stdev := math.Sqrt(variance)

		if stdev == 0 {
			continue
		}

		threshold := mean + 3.0*stdev

		for wTime, count := range windows {
			if float64(count) > threshold && count >= 5 {
				zScore := (float64(count) - mean) / stdev
				allBursts = append(allBursts, BurstStat{
					Pattern:     pat,
					WindowStart: wTime.Format("2006-01-02 15:04:05"),
					Count:       count,
					Avg:         math.Round(mean*100) / 100,
					ZScore:      math.Round(zScore*100) / 100,
				})
			}
		}
	}

	// 5. Sort top 10 bursts by z_score descending
	sort.Slice(allBursts, func(i, j int) bool {
		if allBursts[i].ZScore != allBursts[j].ZScore {
			return allBursts[i].ZScore > allBursts[j].ZScore
		}
		return allBursts[i].Count > allBursts[j].Count
	})

	if len(allBursts) > 10 {
		allBursts = allBursts[:10]
	}

	return allBursts
}

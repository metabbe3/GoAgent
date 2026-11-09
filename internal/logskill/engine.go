package logskill

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// PatternData stores in-memory aggregation of a pattern during log analysis.
type PatternData struct {
	Count  int64
	Sample string
	Level  string
}

// PatternStat represents a pattern entry in the analysis output.
type PatternStat struct {
	Pattern string  `json:"pattern"`
	Count   int64   `json:"count"`
	Percent float64 `json:"percent"`
	Sample  string  `json:"sample"`
	Level   string  `json:"level,omitempty"`
	New     bool    `json:"new,omitempty"`
}

// AnalysisResult represents the final structured JSON output of analyze_logs.
type AnalysisResult struct {
	TotalLines  int64            `json:"total_lines"`
	Levels      map[string]int64 `json:"levels"`
	TopPatterns []PatternStat    `json:"top_patterns"`
	Anomalies   []PatternStat    `json:"anomalies"`
}

// AnalysisOptions configures the log analysis engine.
type AnalysisOptions struct {
	Top         int    `json:"top,omitempty"`
	FilterRegex string `json:"filter_regex,omitempty"`
	MinCount    int    `json:"min_count,omitempty"`
	StoreDir    string `json:"store_dir,omitempty"`
	NumChunks   int    `json:"num_chunks,omitempty"` // For testing chunking
}

var (
	// ISO8601 with 'T' (e.g. 2024-03-29T14:22:10.500Z)
	reISO8601 = regexp.MustCompile(`\b\d{4}[-/]\d{2}[-/]\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?\b`)
	reDate    = regexp.MustCompile(`\b\d{4}[-/]\d{2}[-/]\d{2}\b|\b(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]*\s+\d{1,2}(?:,\s+\d{4})?\b|\b\d{1,2}[-/](?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]*[-/]\d{2,4}\b`)

	// HH:MM:SS
	reTime = regexp.MustCompile(`\b\d{2}:\d{2}:\d{2}(?:[.,]\d+)?\b`)

	// IPv4
	reIPv4 = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`)

	// UUID
	reUUID = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)

	// Long HEX or 0x-prefixed hex
	reHexCandidates = regexp.MustCompile(`\b(?:0x[0-9a-fA-F]+|[0-9a-fA-F]{8,})\b`)

	// Numbers: Durations, Formatted (EU/ID: 1.234,56 / US: 1,234.56), Floats, Integers
	reDuration = regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ms|us|ns|µs|s|m|h)\b`)
	reEuroNum  = regexp.MustCompile(`\b\d{1,3}(?:\.\d{3})+(?:,\d+)?\b`)
	reUSNum    = regexp.MustCompile(`\b\d{1,3}(?:,\d{3})+(?:\.\d+)?\b`)
	reFloat    = regexp.MustCompile(`\b\d+\.\d+\b`)
	reInt      = regexp.MustCompile(`\b\d+\b`)

	// Strings in quotes
	reQuoted = regexp.MustCompile(`"([^"\\]|\\.)*"|'([^'\\]|\\.)*'`)

	// Paths (/foo/bar.js:123 or ./foo/bar.js:123 or /var/log/syslog)
	rePath = regexp.MustCompile(`(?:(?:\.{1,2}|~)?/[a-zA-Z0-9_.-]+)+(?::(?:\d+|{N}))?`)

	// Level detector regex
	reLevel = regexp.MustCompile(`(?:\b|\[)(FATAL|CRITICAL|PANIC|ERROR|ERR|WARN|WARNING|INFO|DEBUG|TRACE)(?:\]|\b|:)`)
)

// NormalizeLine converts a raw log line into a generalized pattern template.
// Order of normalization:
// 1. ISO/date timestamps -> {DATE}
// 2. HH:MM:SS -> {TIME}
// 3. IPv4 -> {IP}
// 4. UUID / hex-panjang -> {UUID}/{HEX}
// 5. angka (int/float, 1.234,56, duration 12ms/3.2s) -> {N}
// 6. string dalam quotes -> {STR} (only if len > 4)
// 7. path /foo/bar.js:123 -> {PATH}
func NormalizeLine(line string) string {
	// 1. ISO timestamps & dates
	s := reISO8601.ReplaceAllString(line, "{DATE}")
	s = reDate.ReplaceAllString(s, "{DATE}")

	// 2. Time
	s = reTime.ReplaceAllString(s, "{TIME}")

	// 3. IPv4
	s = reIPv4.ReplaceAllString(s, "{IP}")

	// 4. UUID
	s = reUUID.ReplaceAllString(s, "{UUID}")

	// 4b. Long HEX (avoid pure digits which belong to numbers)
	s = reHexCandidates.ReplaceAllStringFunc(s, func(match string) string {
		if strings.HasPrefix(match, "0x") || strings.HasPrefix(match, "0X") {
			return "{HEX}"
		}
		// If it contains at least one hex letter [a-fA-F]
		for i := 0; i < len(match); i++ {
			c := match[i]
			if (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
				return "{HEX}"
			}
		}
		return match // Keep pure digits for step 5
	})

	// 5. Numbers: Durations, Formatted numbers, Floats, Integers
	s = reDuration.ReplaceAllString(s, "{N}")
	s = reEuroNum.ReplaceAllString(s, "{N}")
	s = reUSNum.ReplaceAllString(s, "{N}")
	s = reFloat.ReplaceAllString(s, "{N}")
	s = reInt.ReplaceAllString(s, "{N}")

	// 6. Quoted strings (only if inner text length > 4)
	s = reQuoted.ReplaceAllStringFunc(s, func(quoted string) string {
		if len(quoted) >= 2 {
			inner := quoted[1 : len(quoted)-1]
			if len(inner) > 4 {
				return "{STR}"
			}
		}
		return quoted
	})

	// 7. Paths
	s = rePath.ReplaceAllString(s, "{PATH}")

	return strings.TrimSpace(s)
}

// DetectLevel identifies the logging severity level from a log line.
func DetectLevel(line string) string {
	upper := strings.ToUpper(line)

	// Check explicit key-value levels first
	if strings.Contains(upper, "LEVEL=FATAL") || strings.Contains(upper, `"LEVEL":"FATAL"`) || strings.Contains(upper, "LEVEL=CRITICAL") {
		return "FATAL"
	}
	if strings.Contains(upper, "LEVEL=ERROR") || strings.Contains(upper, `"LEVEL":"ERROR"`) || strings.Contains(upper, "LEVEL=ERR") {
		return "ERROR"
	}
	if strings.Contains(upper, "LEVEL=WARN") || strings.Contains(upper, `"LEVEL":"WARN"`) {
		return "WARN"
	}
	if strings.Contains(upper, "LEVEL=INFO") || strings.Contains(upper, `"LEVEL":"INFO"`) {
		return "INFO"
	}
	if strings.Contains(upper, "LEVEL=DEBUG") || strings.Contains(upper, `"LEVEL":"DEBUG"`) || strings.Contains(upper, "LEVEL=TRACE") {
		return "DEBUG"
	}

	match := reLevel.FindStringSubmatch(upper)
	if len(match) > 1 && match[1] != "" {
		switch match[1] {
		case "FATAL", "CRITICAL", "PANIC":
			return "FATAL"
		case "ERROR", "ERR":
			return "ERROR"
		case "WARN", "WARNING":
			return "WARN"
		case "INFO":
			return "INFO"
		case "DEBUG", "TRACE":
			return "DEBUG"
		}
	}

	return "OTHER"
}

type chunkResult struct {
	lineCount int64
	levels    map[string]int64
	patterns  map[string]*PatternData
	err       error
}

// AnalyzeFile performs chunk-parallel log analysis with self-learning pattern detection.
func AnalyzeFile(logPath string, opts AnalysisOptions) (*AnalysisResult, error) {
	fileInfo, err := os.Stat(logPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat log file %s: %w", logPath, err)
	}

	fileSize := fileInfo.Size()
	result := &AnalysisResult{
		Levels: map[string]int64{
			"FATAL": 0,
			"ERROR": 0,
			"WARN":  0,
			"INFO":  0,
			"DEBUG": 0,
			"OTHER": 0,
		},
		TopPatterns: []PatternStat{},
		Anomalies:   []PatternStat{},
	}

	if fileSize == 0 {
		return result, nil
	}

	// Calculate chunk count K
	// K = min(8, size/(8MB)) minimal 1
	k := int(fileSize / (8 * 1024 * 1024))
	if k > 8 {
		k = 8
	}
	if k < 1 {
		k = 1
	}
	if opts.NumChunks > 0 {
		k = opts.NumChunks
	}

	chunkSize := fileSize / int64(k)

	// Compile filter regex if provided
	var filterRe *regexp.Regexp
	if opts.FilterRegex != "" {
		compiled, err := regexp.Compile(opts.FilterRegex)
		if err != nil {
			return nil, fmt.Errorf("invalid filter regex '%s': %w", opts.FilterRegex, err)
		}
		filterRe = compiled
	}

	// Goroutine per chunk (worker pool up to 8)
	var wg sync.WaitGroup
	resultsChan := make(chan chunkResult, k)

	for i := 0; i < k; i++ {
		start := int64(i) * chunkSize
		end := int64(i+1) * chunkSize
		if i == k-1 {
			end = fileSize
		}

		wg.Add(1)
		go func(chunkIndex int, startByte, endByte int64) {
			defer wg.Done()

			f, err := os.Open(logPath)
			if err != nil {
				resultsChan <- chunkResult{err: fmt.Errorf("chunk %d open error: %w", chunkIndex, err)}
				return
			}
			defer f.Close()

			currentPos := startByte

			// Seek to start position
			if startByte > 0 {
				// Check preceding byte to determine if we are already at line start
				prevByte := make([]byte, 1)
				if _, err := f.ReadAt(prevByte, startByte-1); err == nil && prevByte[0] == '\n' {
					// We are exactly at the beginning of a line
					if _, err := f.Seek(startByte, io.SeekStart); err != nil {
						resultsChan <- chunkResult{err: fmt.Errorf("chunk %d seek error: %w", chunkIndex, err)}
						return
					}
				} else {
					// We are in the middle of a line; seek and discard to next newline
					if _, err := f.Seek(startByte, io.SeekStart); err != nil {
						resultsChan <- chunkResult{err: fmt.Errorf("chunk %d seek error: %w", chunkIndex, err)}
						return
					}
					reader := bufio.NewReaderSize(f, 64*1024)
					discarded, err := reader.ReadBytes('\n')
					currentPos += int64(len(discarded))
					if err != nil {
						// Reached EOF while skipping
						resultsChan <- chunkResult{
							levels:   make(map[string]int64),
							patterns: make(map[string]*PatternData),
						}
						return
					}
					// Seek file to currentPos for a clean new reader
					if _, err := f.Seek(currentPos, io.SeekStart); err != nil {
						resultsChan <- chunkResult{err: fmt.Errorf("chunk %d seek error: %w", chunkIndex, err)}
						return
					}
				}
			} else {
				if _, err := f.Seek(0, io.SeekStart); err != nil {
					resultsChan <- chunkResult{err: fmt.Errorf("chunk %d seek error: %w", chunkIndex, err)}
					return
				}
			}

			reader := bufio.NewReaderSize(f, 1024*1024)
			cRes := chunkResult{
				levels:   make(map[string]int64),
				patterns: make(map[string]*PatternData),
			}

			for {
				if chunkIndex < k-1 && currentPos >= endByte {
					break
				}

				lineBytes, err := reader.ReadBytes('\n')
				if len(lineBytes) > 0 {
					currentPos += int64(len(lineBytes))
					lineStr := strings.TrimRight(string(lineBytes), "\r\n")

					// Apply filter if requested
					if filterRe != nil && !filterRe.MatchString(lineStr) {
						if err != nil {
							break
						}
						continue
					}

					cRes.lineCount++
					lvl := DetectLevel(lineStr)
					cRes.levels[lvl]++

					pat := NormalizeLine(lineStr)
					if entry, exists := cRes.patterns[pat]; exists {
						entry.Count++
					} else {
						sample := lineStr
						if len(sample) > 200 {
							sample = sample[:200] + "..."
						}
						cRes.patterns[pat] = &PatternData{
							Count:  1,
							Sample: sample,
							Level:  lvl,
						}
					}
				}

				if err != nil {
					if err != io.EOF {
						cRes.err = err
					}
					break
				}
			}

			resultsChan <- cRes
		}(i, start, end)
	}

	wg.Wait()
	close(resultsChan)

	// Step 3: Merge chunk results
	totalPatterns := make(map[string]*PatternData)
	for res := range resultsChan {
		if res.err != nil {
			return nil, res.err
		}
		result.TotalLines += res.lineCount
		for lvl, count := range res.levels {
			result.Levels[lvl] += count
		}
		for pat, data := range res.patterns {
			if existing, exists := totalPatterns[pat]; exists {
				existing.Count += data.Count
			} else {
				totalPatterns[pat] = &PatternData{
					Count:  data.Count,
					Sample: data.Sample,
					Level:  data.Level,
				}
			}
		}
	}

	// Update self-learning pattern store
	storePath, err := GetStorePath(logPath, opts.StoreDir)
	var newPatterns map[string]bool
	if err == nil {
		newPatterns, _ = UpdateLearnedPatterns(storePath, totalPatterns, time.Now())
	}
	if newPatterns == nil {
		newPatterns = make(map[string]bool)
	}

	// Sort patterns by count descending
	allStats := make([]PatternStat, 0, len(totalPatterns))
	for pat, data := range totalPatterns {
		var pct float64
		if result.TotalLines > 0 {
			pct = math.Round((float64(data.Count)/float64(result.TotalLines))*10000) / 100
		}
		isNew := newPatterns[pat]

		allStats = append(allStats, PatternStat{
			Pattern: pat,
			Count:   data.Count,
			Percent: pct,
			Sample:  data.Sample,
			Level:   data.Level,
			New:     isNew,
		})
	}

	sort.Slice(allStats, func(i, j int) bool {
		if allStats[i].Count != allStats[j].Count {
			return allStats[i].Count > allStats[j].Count
		}
		return allStats[i].Pattern < allStats[j].Pattern
	})

	// Top N patterns
	topN := opts.Top
	if topN <= 0 {
		topN = 10
	}
	if topN > len(allStats) {
		topN = len(allStats)
	}
	result.TopPatterns = append(result.TopPatterns, allStats[:topN]...)

	// Step 5: Anomaly detection:
	// Pola dengan count <= min_count (default 1) ATAU pola yang BELUM ada di pattern store (New == true)
	minCount := int64(opts.MinCount)
	if minCount <= 0 {
		minCount = 1
	}

	for _, stat := range allStats {
		if stat.Count <= minCount || stat.New {
			result.Anomalies = append(result.Anomalies, stat)
		}
	}

	return result, nil
}

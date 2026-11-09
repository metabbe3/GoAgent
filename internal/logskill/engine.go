package logskill

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
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
	Format       string            `json:"format"`
	TotalLines   int64             `json:"total_lines"`
	Levels       map[string]int64  `json:"levels"`
	TopPatterns  []PatternStat     `json:"top_patterns"`
	Anomalies    []PatternStat     `json:"anomalies"`
	Bursts       []BurstStat       `json:"bursts,omitempty"`
	Correlations []CorrelationStat `json:"correlations,omitempty"`
	Changes      []ChangeStat      `json:"changes,omitempty"`
}

// AnalysisOptions configures the log analysis engine.
type AnalysisOptions struct {
	Top             int    `json:"top,omitempty"`
	FilterRegex     string `json:"filter_regex,omitempty"`
	MinCount        int    `json:"min_count,omitempty"`
	StoreDir        string `json:"store_dir,omitempty"`
	NumChunks       int    `json:"num_chunks,omitempty"` // For testing chunking
	CompareBaseline bool   `json:"compare_baseline,omitempty"`
	Window          string `json:"window,omitempty"` // "1h", "24h", or empty for full file
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

	// Formats detection regexes
	reApacheCombined = regexp.MustCompile(`^(\S+)\s+\S+\s+\S+\s+\[([^\]]+)\]\s+"([A-Z]+)\s+([^"\s]+)[^"]*"\s+(\d{3})\s+(\d+|-)`)
	reSyslogHeader   = regexp.MustCompile(`^(?:<\d+>)?(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)\s+\d{1,2}\s+\d{2}:\d{2}:\d{2}\b`)
	reLogfmtKeyVal   = regexp.MustCompile(`\b([a-zA-Z0-9_.-]+)=(?:"[^"]*"|'[^']*'|\S+)`)
	reApacheTS       = regexp.MustCompile(`\[(\d{2}/[A-Za-z]{3}/\d{4}:\d{2}:\d{2}:\d{2}(?:\s+[+-]\d{4})?)\]`)
	reSyslogTS       = regexp.MustCompile(`\b(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)\s+(\d{1,2})\s+(\d{2}:\d{2}:\d{2})\b`)
	reTimestampGen   = regexp.MustCompile(`\b\d{4}[-/]\d{2}[-/]\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?\b`)
)

// NormalizeLine converts a raw log line into a generalized pattern template.
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

	// 4b. Long HEX
	s = reHexCandidates.ReplaceAllStringFunc(s, func(match string) string {
		if strings.HasPrefix(match, "0x") || strings.HasPrefix(match, "0X") {
			return "{HEX}"
		}
		for i := 0; i < len(match); i++ {
			c := match[i]
			if (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
				return "{HEX}"
			}
		}
		return match
	})

	// 5. Numbers
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

// DetectFormat samples the first 100 non-empty lines to detect the log format:
// "json", "logfmt", "apache/nginx", "syslog", or fallback "raw".
func DetectFormat(logPath string) string {
	f, err := os.Open(logPath)
	if err != nil {
		return "raw"
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 512*1024)

	var lines []string
	for scanner.Scan() && len(lines) < 100 {
		t := strings.TrimSpace(scanner.Text())
		if t != "" {
			lines = append(lines, t)
		}
	}

	if len(lines) == 0 {
		return "raw"
	}

	total := len(lines)
	jsonCount := 0
	logfmtCount := 0
	apacheCount := 0
	syslogCount := 0

	for _, line := range lines {
		if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
			var dummy map[string]any
			if json.Unmarshal([]byte(line), &dummy) == nil {
				jsonCount++
			}
		}
		if reApacheCombined.MatchString(line) {
			apacheCount++
		}
		if reSyslogHeader.MatchString(line) {
			syslogCount++
		}
		pairs := reLogfmtKeyVal.FindAllString(line, -1)
		if len(pairs) >= 2 {
			logfmtCount++
		}
	}

	if float64(jsonCount)/float64(total) >= 0.7 {
		return "json"
	}
	if float64(apacheCount)/float64(total) >= 0.6 {
		return "apache/nginx"
	}
	if float64(syslogCount)/float64(total) >= 0.6 {
		return "syslog"
	}
	if float64(logfmtCount)/float64(total) >= 0.6 {
		return "logfmt"
	}

	return "raw"
}

// ParseTimestamp extracts a timestamp from a raw log line across common formats.
func ParseTimestamp(line string) (time.Time, bool) {
	// 1. ISO8601 / Date + Time / RFC3339
	if m := reTimestampGen.FindString(line); m != "" {
		if t, ok := parseTimeString(m); ok {
			return t, true
		}
	}

	// 2. Apache combined format: [10/Oct/2000:13:55:36 -0700]
	if m := reApacheTS.FindStringSubmatch(line); len(m) > 1 {
		if t, err := time.Parse("02/Jan/2006:15:04:05 -0700", m[1]); err == nil {
			return t, true
		}
		if t, err := time.Parse("02/Jan/2006:15:04:05", m[1]); err == nil {
			return t, true
		}
	}

	// 3. Syslog format: Oct 11 22:14:15
	if m := reSyslogTS.FindString(line); m != "" {
		currYear := time.Now().Year()
		withYear := fmt.Sprintf("%d %s", currYear, m)
		if t, err := time.Parse("2006 Jan _2 15:04:05", withYear); err == nil {
			return t, true
		}
	}

	return time.Time{}, false
}

func parseTimeString(s string) (time.Time, bool) {
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05.999",
		"2006-01-02 15:04:05",
		"2006/01/02 15:04:05",
		"2006-01-02T15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// ProcessLine extracts pattern, level, and timestamp based on detected format.
func ProcessLine(line string, format string) (string, string, time.Time, bool) {
	var (
		pat   string
		lvl   string
		ts    time.Time
		hasTS bool
	)

	switch format {
	case "json":
		var j map[string]any
		if err := json.Unmarshal([]byte(line), &j); err == nil {
			// Extract level
			for _, k := range []string{"level", "lvl", "severity", "log_level", "log.level"} {
				if val, ok := j[k]; ok {
					if s, ok := val.(string); ok && s != "" {
						u := strings.ToUpper(s)
						switch u {
						case "FATAL", "CRITICAL", "PANIC":
							lvl = "FATAL"
						case "ERROR", "ERR":
							lvl = "ERROR"
						case "WARN", "WARNING":
							lvl = "WARN"
						case "INFO":
							lvl = "INFO"
						case "DEBUG", "TRACE":
							lvl = "DEBUG"
						}
						break
					}
				}
			}

			// Extract timestamp
			for _, k := range []string{"ts", "time", "timestamp", "@timestamp", "datetime"} {
				if val, ok := j[k]; ok {
					if s, ok := val.(string); ok {
						if t, ok := parseTimeString(s); ok {
							ts = t
							hasTS = true
							break
						}
					} else if f, ok := val.(float64); ok {
						if f > 1e11 {
							ts = time.UnixMilli(int64(f))
							hasTS = true
							break
						} else if f > 1e8 {
							ts = time.Unix(int64(f), 0)
							hasTS = true
							break
						}
					}
				}
			}

			// Extract message for template pattern
			for _, k := range []string{"msg", "message", "error", "text", "payload"} {
				if val, ok := j[k]; ok {
					if s, ok := val.(string); ok && s != "" {
						pat = NormalizeLine(s)
						break
					}
				}
			}
		}

	case "apache/nginx":
		if m := reApacheCombined.FindStringSubmatch(line); len(m) >= 7 {
			status, _ := strconv.Atoi(m[5])
			if status >= 500 {
				lvl = "ERROR"
			} else if status >= 400 {
				lvl = "WARN"
			} else {
				lvl = "INFO"
			}

			if t, ok := ParseTimestamp(line); ok {
				ts = t
				hasTS = true
			}

			// Normalized pattern: {IP} - - [{DATE}] "METHOD PATH" STATUS {N}
			pat = fmt.Sprintf("{IP} - - [{DATE}] \"%s %s\" %s {N}", m[3], NormalizeLine(m[4]), m[5])
		}

	case "logfmt":
		if t, ok := ParseTimestamp(line); ok {
			ts = t
			hasTS = true
		}
		// Extract level if present: level=info
		if m := regexp.MustCompile(`(?i)\b(?:level|lvl)=([a-zA-Z]+)\b`).FindStringSubmatch(line); len(m) > 1 {
			u := strings.ToUpper(m[1])
			switch u {
			case "FATAL", "CRITICAL", "PANIC":
				lvl = "FATAL"
			case "ERROR", "ERR":
				lvl = "ERROR"
			case "WARN", "WARNING":
				lvl = "WARN"
			case "INFO":
				lvl = "INFO"
			case "DEBUG", "TRACE":
				lvl = "DEBUG"
			}
		}
		pat = NormalizeLine(line)
	}

	// Fallbacks
	if lvl == "" {
		lvl = DetectLevel(line)
	}
	if pat == "" {
		pat = NormalizeLine(line)
	}
	if !hasTS {
		if t, ok := ParseTimestamp(line); ok {
			ts = t
			hasTS = true
		}
	}

	return pat, lvl, ts, hasTS
}

// findMaxTimestamp scans lines to find the maximum timestamp in the file.
func findMaxTimestamp(logPath string, format string) (time.Time, bool) {
	f, err := os.Open(logPath)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()

	// If file is large, seek to near the end (last 512KB)
	fi, err := f.Stat()
	if err == nil && fi.Size() > 512*1024 {
		_, _ = f.Seek(-512*1024, io.SeekEnd)
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 512*1024)

	var maxTS time.Time
	found := false

	for scanner.Scan() {
		line := scanner.Text()
		_, _, ts, hasTS := ProcessLine(line, format)
		if hasTS {
			if !found || ts.After(maxTS) {
				maxTS = ts
				found = true
			}
		}
	}

	return maxTS, found
}

type chunkResult struct {
	lineCount     int64
	levels        map[string]int64
	patterns      map[string]*PatternData
	timedPatterns []TimedPattern
	err           error
}

// AnalyzeFile performs chunk-parallel log analysis with format autodetect,
// burst detection, correlation analysis, and self-learning pattern detection.
func AnalyzeFile(logPath string, opts AnalysisOptions) (*AnalysisResult, error) {
	fileInfo, err := os.Stat(logPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat log file %s: %w", logPath, err)
	}

	format := DetectFormat(logPath)
	fileSize := fileInfo.Size()

	result := &AnalysisResult{
		Format: format,
		Levels: map[string]int64{
			"FATAL": 0,
			"ERROR": 0,
			"WARN":  0,
			"INFO":  0,
			"DEBUG": 0,
			"OTHER": 0,
		},
		TopPatterns:  []PatternStat{},
		Anomalies:    []PatternStat{},
		Bursts:       []BurstStat{},
		Correlations: []CorrelationStat{},
		Changes:      []ChangeStat{},
	}

	if fileSize == 0 {
		return result, nil
	}

	// Window cutoff calculation if window is specified (e.g. "1h", "24h")
	var cutoffTime time.Time
	hasWindowFilter := false
	if opts.Window != "" {
		if dur, err := time.ParseDuration(opts.Window); err == nil && dur > 0 {
			if maxTS, ok := findMaxTimestamp(logPath, format); ok {
				cutoffTime = maxTS.Add(-dur)
				hasWindowFilter = true
			}
		}
	}

	// Calculate chunk count K = min(8, size/(8MB)) minimal 1
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

	var filterRe *regexp.Regexp
	if opts.FilterRegex != "" {
		compiled, err := regexp.Compile(opts.FilterRegex)
		if err != nil {
			return nil, fmt.Errorf("invalid filter regex '%s': %w", opts.FilterRegex, err)
		}
		filterRe = compiled
	}

	var wg sync.WaitGroup
	chunkResults := make([]chunkResult, k)

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
				chunkResults[chunkIndex] = chunkResult{err: fmt.Errorf("chunk %d open error: %w", chunkIndex, err)}
				return
			}
			defer f.Close()

			currentPos := startByte

			// Seek to start position
			if startByte > 0 {
				prevByte := make([]byte, 1)
				if _, err := f.ReadAt(prevByte, startByte-1); err == nil && prevByte[0] == '\n' {
					if _, err := f.Seek(startByte, io.SeekStart); err != nil {
						chunkResults[chunkIndex] = chunkResult{err: fmt.Errorf("chunk %d seek error: %w", chunkIndex, err)}
						return
					}
				} else {
					if _, err := f.Seek(startByte, io.SeekStart); err != nil {
						chunkResults[chunkIndex] = chunkResult{err: fmt.Errorf("chunk %d seek error: %w", chunkIndex, err)}
						return
					}
					reader := bufio.NewReaderSize(f, 64*1024)
					discarded, err := reader.ReadBytes('\n')
					currentPos += int64(len(discarded))
					if err != nil {
						chunkResults[chunkIndex] = chunkResult{
							levels:   make(map[string]int64),
							patterns: make(map[string]*PatternData),
						}
						return
					}
					if _, err := f.Seek(currentPos, io.SeekStart); err != nil {
						chunkResults[chunkIndex] = chunkResult{err: fmt.Errorf("chunk %d seek error: %w", chunkIndex, err)}
						return
					}
				}
			} else {
				if _, err := f.Seek(0, io.SeekStart); err != nil {
					chunkResults[chunkIndex] = chunkResult{err: fmt.Errorf("chunk %d seek error: %w", chunkIndex, err)}
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

					if filterRe != nil && !filterRe.MatchString(lineStr) {
						if err != nil {
							break
						}
						continue
					}

					pat, lvl, ts, hasTS := ProcessLine(lineStr, format)

					// Filter by time window if requested
					if hasWindowFilter && hasTS && ts.Before(cutoffTime) {
						if err != nil {
							break
						}
						continue
					}

					cRes.lineCount++
					cRes.levels[lvl]++

					if hasTS {
						cRes.timedPatterns = append(cRes.timedPatterns, TimedPattern{
							Timestamp: ts,
							Pattern:   pat,
						})
					}

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

			chunkResults[chunkIndex] = cRes
		}(i, start, end)
	}

	wg.Wait()

	// Merge chunk results
	totalPatterns := make(map[string]*PatternData)
	var allTimedPatterns []TimedPattern

	for _, res := range chunkResults {
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
		allTimedPatterns = append(allTimedPatterns, res.timedPatterns...)
	}

	// Run Burst detection and Correlation analysis
	result.Bursts = DetectBursts(allTimedPatterns)
	result.Correlations = DetectCorrelations(allTimedPatterns)

	// Baseline comparison if requested
	storePath, err := GetStorePath(logPath, opts.StoreDir)
	if opts.CompareBaseline && err == nil {
		todayStr := time.Now().Format("2006-01-02")
		changes, _ := CompareWithBaseline(storePath, totalPatterns, todayStr)
		if changes != nil {
			result.Changes = changes
		}
	}

	// Update self-learning pattern store
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

	topN := opts.Top
	if topN <= 0 {
		topN = 10
	}
	if topN > len(allStats) {
		topN = len(allStats)
	}
	result.TopPatterns = append(result.TopPatterns, allStats[:topN]...)

	// Anomaly detection
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

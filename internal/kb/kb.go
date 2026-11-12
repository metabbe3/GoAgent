// Package kb implements a generic RAG knowledge base: documents are chunked,
// embedded, and persisted to kb.jsonl; searches embed the query and rank
// chunks by cosine similarity with a similarity floor.
package kb

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	embedBatchSize = 16
	fileName       = "kb.jsonl"
)

var (
	chunkSize       = 900
	chunkOverlap    = 100
	maxChunksPerDoc = 150
	maxChunksTotal  = 1000
)

// Embedder embeds a batch of texts into float32 vectors.
type Embedder interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
}

// Chunk is one embedded piece of a document.
type Chunk struct {
	Doc   string    `json:"doc"`
	Sum   uint32    `json:"sum"`
	Seq   int       `json:"seq"`
	Text  string    `json:"text"`
	Vec   []float32 `json:"vec"`
	Model string    `json:"model"`
	TS    int64     `json:"ts"`
}

// Index manages persistent knowledge chunks and vector search.
type Index struct {
	path   string
	model  string
	embed  Embedder
	mu     sync.RWMutex
	chunks []Chunk
	ingest chan struct{}
}

// NewIndex initializes or loads an index from dir ("" = in-memory only).
func NewIndex(dir, model string, embed Embedder) *Index {
	ix := &Index{
		model:  model,
		embed:  embed,
		ingest: make(chan struct{}, 1),
	}
	if dir != "" {
		ix.path = filepath.Join(dir, fileName)
		ix.load()
	}
	return ix
}

func (ix *Index) load() {
	data, err := os.ReadFile(ix.path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var c Chunk
		if err := json.Unmarshal([]byte(line), &c); err == nil {
			ix.chunks = append(ix.chunks, c)
		}
	}
}

// Ingest chunks and embeds a document asynchronously in the background.
func (ix *Index) Ingest(doc, text string) {
	go func() {
		ix.ingest <- struct{}{}
		defer func() { <-ix.ingest }()
		if err := ix.ingestNow(context.Background(), doc, text); err != nil {
			log.Printf("[kb] ingest %s failed: %v", doc, err)
		}
	}()
}

// Save ingests a document synchronously so tools can report results.
func (ix *Index) Save(ctx context.Context, doc, text string) error {
	if ix.embed == nil {
		return fmt.Errorf("embedding not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	select {
	case ix.ingest <- struct{}{}:
		defer func() { <-ix.ingest }()
		return ix.ingestNow(ctx, doc, text)
	case <-ctx.Done():
		return fmt.Errorf("knowledge base busy: %w", ctx.Err())
	}
}

func (ix *Index) ingestNow(ctx context.Context, doc, text string) error {
	if ix.embed == nil {
		return fmt.Errorf("embedding not configured")
	}
	chunks := chunkText(doc, text)
	if len(chunks) == 0 {
		return fmt.Errorf("no text to index")
	}

	vecs, err := ix.embedAll(ctx, chunks)
	if err != nil {
		return err
	}

	sum := docSum(doc)
	now := time.Now().UnixNano()
	newChunks := make([]Chunk, len(chunks))
	for i, t := range chunks {
		newChunks[i] = Chunk{
			Doc:   doc,
			Sum:   sum,
			Seq:   i,
			Text:  t,
			Vec:   vecs[i],
			Model: ix.model,
			TS:    now,
		}
	}

	ix.mu.Lock()
	defer ix.mu.Unlock()

	// Replace existing document version if any
	kept := ix.chunks[:0]
	for _, c := range ix.chunks {
		if c.Sum != sum {
			kept = append(kept, c)
		}
	}
	ix.chunks = append(kept, newChunks...)

	// Global cap enforcement
	for len(ix.chunks) > maxChunksTotal {
		ix.evictOldestLocked()
	}

	return ix.save()
}

func (ix *Index) evictOldestLocked() {
	minTS := int64(-1)
	for _, c := range ix.chunks {
		if minTS == -1 || c.TS < minTS {
			minTS = c.TS
		}
	}
	kept := ix.chunks[:0]
	for _, c := range ix.chunks {
		if c.TS != minTS {
			kept = append(kept, c)
		}
	}
	ix.chunks = kept
}

// Search ranks indexed chunks against query by cosine similarity.
// Chunks below the 0.35 similarity floor are filtered out.
func (ix *Index) Search(ctx context.Context, query string, k int) ([]Chunk, error) {
	ix.mu.RLock()
	total := len(ix.chunks)
	ix.mu.RUnlock()
	if total == 0 {
		return nil, nil
	}
	if ix.embed == nil {
		return nil, fmt.Errorf("embedding not configured")
	}

	vecs, err := ix.embed.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("knowledge base query embed failed: %w", err)
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("knowledge base query embed: want 1 vector, got %d", len(vecs))
	}
	q := vecs[0]

	ix.mu.RLock()
	defer ix.mu.RUnlock()

	type scored struct {
		c Chunk
		s float64
	}
	all := make([]scored, 0, len(ix.chunks))
	for _, c := range ix.chunks {
		all = append(all, scored{c: c, s: cosine(q, c.Vec)})
	}

	sort.Slice(all, func(i, j int) bool { return all[i].s > all[j].s })

	const minCosine = 0.35
	out := make([]Chunk, 0, k)
	for _, s := range all {
		if len(out) >= k {
			break
		}
		if s.s < minCosine {
			break
		}
		out = append(out, s.c)
	}
	return out, nil
}

// DocStat summarizes one document in the index.
type DocStat struct {
	Doc    string `json:"doc"`
	Chunks int    `json:"chunks"`
	Chars  int    `json:"chars"`
	TS     int64  `json:"ts"`
}

// ListDocs aggregates document statistics, newest first.
func (ix *Index) ListDocs() []DocStat {
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	byDoc := make(map[string]*DocStat)
	for _, c := range ix.chunks {
		d, ok := byDoc[c.Doc]
		if !ok {
			d = &DocStat{Doc: c.Doc, TS: c.TS}
			byDoc[c.Doc] = d
		}
		d.Chunks++
		d.Chars += len(c.Text)
		if c.TS > d.TS {
			d.TS = c.TS
		}
	}

	out := make([]DocStat, 0, len(byDoc))
	for _, d := range byDoc {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS > out[j].TS })
	return out
}

// DeleteDoc deletes all chunks of the specified document title.
func (ix *Index) DeleteDoc(doc string) (int, bool) {
	sum := docSum(doc)
	ix.mu.Lock()
	defer ix.mu.Unlock()

	kept := ix.chunks[:0]
	n := 0
	for _, c := range ix.chunks {
		if c.Sum == sum {
			n++
			continue
		}
		kept = append(kept, c)
	}
	if n == 0 {
		return 0, false
	}
	ix.chunks = kept
	_ = ix.save()
	return n, true
}

func (ix *Index) embedAll(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += embedBatchSize {
		end := start + embedBatchSize
		if end > len(texts) {
			end = len(texts)
		}
		vecs, err := ix.embed.Embed(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		if len(vecs) != end-start {
			return nil, fmt.Errorf("batch embed mismatch: want %d, got %d", end-start, len(vecs))
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (ix *Index) save() error {
	if ix.path == "" {
		return nil
	}
	var sb strings.Builder
	for _, c := range ix.chunks {
		b, err := json.Marshal(c)
		if err != nil {
			return err
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	dir := filepath.Dir(ix.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := ix.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ix.path)
}

func chunkText(doc, text string) []string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) == 0 {
		return nil
	}
	if len(runes) <= chunkSize {
		return []string{string(runes)}
	}
	var out []string
	for start := 0; start < len(runes) && len(out) < maxChunksPerDoc; start += chunkSize - chunkOverlap {
		end := start + chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[start:end]))
		if end == len(runes) {
			break
		}
	}
	return out
}

func docSum(doc string) uint32 {
	h := sha256.Sum256([]byte(doc))
	return uint32(h[0]) | uint32(h[1])<<8 | uint32(h[2])<<16 | uint32(h[3])<<24
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return -1
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

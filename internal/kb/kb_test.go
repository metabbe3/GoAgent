package kb

import (
	"context"
	"strings"
	"testing"
)

type mockEmbedder struct {
	vecs map[string][]float32
}

func (m *mockEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i, in := range inputs {
		if v, ok := m.vecs[in]; ok {
			out[i] = v
		} else {
			// Default 2D vector
			out[i] = []float32{0.5, 0.5}
		}
	}
	return out, nil
}

func TestSimilarityFloorFilter(t *testing.T) {
	mock := &mockEmbedder{
		vecs: map[string][]float32{
			"alpha document content": {1.0, 0.0},
			"beta document content":  {0.0, 1.0},
			"search query":           {0.9, 0.1},
		},
	}

	ix := NewIndex("", "test-model", mock)
	ctx := context.Background()

	if err := ix.Save(ctx, "alpha", "alpha document content"); err != nil {
		t.Fatalf("save alpha failed: %v", err)
	}
	if err := ix.Save(ctx, "beta", "beta document content"); err != nil {
		t.Fatalf("save beta failed: %v", err)
	}

	hits, err := ix.Search(ctx, "search query", 5)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}

	if len(hits) != 1 {
		t.Fatalf("expected exactly 1 hit above 0.35 floor, got %d", len(hits))
	}
	if hits[0].Doc != "alpha" {
		t.Errorf("expected alpha to pass floor, got %s", hits[0].Doc)
	}
}

func TestIndexReplaceAndDelete(t *testing.T) {
	mock := &mockEmbedder{
		vecs: map[string][]float32{
			"v1 content": {0.8, 0.6},
			"v2 content": {0.8, 0.6},
		},
	}
	ix := NewIndex("", "test-model", mock)
	ctx := context.Background()

	_ = ix.Save(ctx, "doc1", "v1 content")
	if len(ix.chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(ix.chunks))
	}

	// Re-ingest same title replaces previous version
	_ = ix.Save(ctx, "doc1", "v2 content")
	if len(ix.chunks) != 1 {
		t.Fatalf("expected 1 chunk after replace, got %d", len(ix.chunks))
	}
	if ix.chunks[0].Text != "v2 content" {
		t.Errorf("expected replaced text 'v2 content', got %s", ix.chunks[0].Text)
	}

	// Delete
	n, ok := ix.DeleteDoc("doc1")
	if !ok || n != 1 || len(ix.chunks) != 0 {
		t.Errorf("delete doc1 failed: n=%d, ok=%v", n, ok)
	}
}

func TestUnconfiguredEmbeddingError(t *testing.T) {
	ix := NewIndex("", "test-model", nil)
	err := ix.Save(context.Background(), "doc", "text")
	if err == nil || !strings.Contains(err.Error(), "embedding not configured") {
		t.Errorf("expected 'embedding not configured' error, got %v", err)
	}
}

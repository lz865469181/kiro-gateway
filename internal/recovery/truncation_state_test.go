package recovery

import (
	"sync"
	"testing"
	"time"
)

func TestStoreOneShotAndStats(t *testing.T) {
	s := NewStore(0)
	s.SaveToolTruncation("toolu_1", "write", map[string]any{"size_bytes": 5000, "reason": "cut"})
	hash := s.SaveContentTruncation("truncated content")
	if len(hash) != 16 {
		t.Fatalf("hash length %d", len(hash))
	}
	if got := s.Stats(); got != (CacheStats{ToolTruncations: 1, ContentTruncations: 1, Total: 2}) {
		t.Fatalf("stats %#v", got)
	}
	tool, ok := s.GetToolTruncation("toolu_1")
	if !ok || tool.ToolName != "write" || tool.Timestamp.IsZero() {
		t.Fatalf("tool %#v %v", tool, ok)
	}
	if _, ok := s.GetToolTruncation("toolu_1"); ok {
		t.Fatal("tool returned twice")
	}
	content, ok := s.GetContentTruncation("truncated content")
	if !ok || content.MessageHash != hash || content.ContentPreview != "truncated content" {
		t.Fatalf("content %#v %v", content, ok)
	}
	if _, ok := s.GetContentTruncation("truncated content"); ok {
		t.Fatal("content returned twice")
	}
}

func TestStoreContentHashPrefixAndUnicodePreview(t *testing.T) {
	s := NewStore(0)
	prefix := ""
	for range 500 {
		prefix += "界"
	}
	hash := s.SaveContentTruncation(prefix + "A")
	if other := contentHash(prefix + "B"); other != hash {
		t.Fatalf("same 500-char prefix differs: %q %q", hash, other)
	}
	info, ok := s.GetContentTruncation(prefix + "B")
	if !ok || len([]rune(info.ContentPreview)) != 200 {
		t.Fatalf("preview %#v %v", info, ok)
	}
}

func TestStoreExpiry(t *testing.T) {
	now := time.Unix(100, 0)
	s := NewStore(time.Minute)
	s.now = func() time.Time { return now }
	s.SaveToolTruncation("id", "tool", nil)
	s.SaveContentTruncation("content")
	now = now.Add(time.Minute)
	if _, ok := s.GetToolTruncation("id"); ok {
		t.Fatal("expired tool returned")
	}
	if _, ok := s.GetContentTruncation("content"); ok {
		t.Fatal("expired content returned")
	}
	if got := s.Stats(); got.Total != 0 {
		t.Fatalf("expired stats %#v", got)
	}
}

func TestStoreConcurrentConsume(t *testing.T) {
	s := NewStore(0)
	const count = 100
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := time.Unix(0, int64(i)).String()
			s.SaveToolTruncation(id, id, nil)
			if _, ok := s.GetToolTruncation(id); !ok {
				t.Errorf("missing %s", id)
			}
		}()
	}
	wg.Wait()
	if got := s.Stats(); got.Total != 0 {
		t.Fatalf("remaining %#v", got)
	}
}

func TestBoundedStoreEvictsOldest(t *testing.T) {
	store := newBoundedStore(time.Hour, 2)
	now := time.Unix(1000, 0)
	store.now = func() time.Time { return now }
	store.SaveToolTruncation("first", "tool", nil)
	now = now.Add(time.Second)
	store.SaveToolTruncation("second", "tool", nil)
	now = now.Add(time.Second)
	store.SaveContentTruncation("third")
	if _, ok := store.GetToolTruncation("first"); ok {
		t.Fatal("oldest entry was not evicted")
	}
	if stats := store.Stats(); stats.Total != 2 {
		t.Fatalf("stats=%+v", stats)
	}
}

func TestTruncationNotices(t *testing.T) {
	result := GenerateTruncationToolResult("write", "toolu_1", map[string]any{"size_bytes": 1})
	if result["type"] != "tool_result" || result["tool_use_id"] != "toolu_1" || result["is_error"] != true || result["content"] != truncationToolNotice {
		t.Fatalf("result %#v", result)
	}
	if GenerateTruncationUserMessage() != truncationUserNotice || !ShouldInjectRecovery(true) || ShouldInjectRecovery(false) {
		t.Fatal("recovery notice behavior changed")
	}
}

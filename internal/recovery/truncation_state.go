// Kiro Gateway
// Copyright (C) 2025 Jwadow
// SPDX-License-Identifier: AGPL-3.0-or-later

package recovery

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// ToolTruncationInfo records a tool call that was cut off upstream.
type ToolTruncationInfo struct {
	ToolCallID     string
	ToolName       string
	TruncationInfo map[string]any
	Timestamp      time.Time
}

// ContentTruncationInfo records a truncated assistant message.
type ContentTruncationInfo struct {
	MessageHash    string
	ContentPreview string
	Timestamp      time.Time
}

type CacheStats struct {
	ToolTruncations    int
	ContentTruncations int
	Total              int
}

type timedTool struct {
	info      ToolTruncationInfo
	expiresAt time.Time
}

type timedContent struct {
	info      ContentTruncationInfo
	expiresAt time.Time
}

// Store is a concurrent, one-shot truncation cache. A non-positive TTL keeps
// entries indefinitely. Expired entries are removed lazily by all operations.
type Store struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
	tools      map[string]timedTool
	content    map[string]timedContent
}

func NewStore(ttl time.Duration) *Store {
	return &Store{
		ttl: ttl, now: time.Now,
		tools: make(map[string]timedTool), content: make(map[string]timedContent),
	}
}

func newBoundedStore(ttl time.Duration, maxEntries int) *Store {
	s := NewStore(ttl)
	s.maxEntries = maxEntries
	return s
}

func (s *Store) expiry(now time.Time) time.Time {
	if s.ttl <= 0 {
		return time.Time{}
	}
	return now.Add(s.ttl)
}

func (s *Store) purgeExpiredLocked(now time.Time) {
	for id, entry := range s.tools {
		if !entry.expiresAt.IsZero() && !now.Before(entry.expiresAt) {
			delete(s.tools, id)
		}
	}
	for hash, entry := range s.content {
		if !entry.expiresAt.IsZero() && !now.Before(entry.expiresAt) {
			delete(s.content, hash)
		}
	}
}

func cloneMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	result := make(map[string]any, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func (s *Store) enforceLimitLocked() {
	for s.maxEntries > 0 && len(s.tools)+len(s.content) >= s.maxEntries {
		var oldestTool, oldestContent string
		var oldest time.Time
		for id, entry := range s.tools {
			if oldest.IsZero() || entry.info.Timestamp.Before(oldest) {
				oldest, oldestTool, oldestContent = entry.info.Timestamp, id, ""
			}
		}
		for hash, entry := range s.content {
			if oldest.IsZero() || entry.info.Timestamp.Before(oldest) {
				oldest, oldestTool, oldestContent = entry.info.Timestamp, "", hash
			}
		}
		if oldestTool != "" {
			delete(s.tools, oldestTool)
		} else if oldestContent != "" {
			delete(s.content, oldestContent)
		} else {
			break
		}
	}
}

func (s *Store) SaveToolTruncation(toolCallID, toolName string, truncationInfo map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purgeExpiredLocked(now)
	s.enforceLimitLocked()
	s.tools[toolCallID] = timedTool{
		info:      ToolTruncationInfo{ToolCallID: toolCallID, ToolName: toolName, TruncationInfo: cloneMap(truncationInfo), Timestamp: now},
		expiresAt: s.expiry(now),
	}
}

// GetToolTruncation atomically retrieves and removes an entry.
func (s *Store) GetToolTruncation(toolCallID string) (ToolTruncationInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purgeExpiredLocked(now)
	entry, ok := s.tools[toolCallID]
	if ok {
		delete(s.tools, toolCallID)
	}
	return entry.info, ok
}

func contentHash(content string) string {
	runes := []rune(content)
	if len(runes) > 500 {
		runes = runes[:500]
	}
	sum := sha256.Sum256([]byte(string(runes)))
	return hex.EncodeToString(sum[:])[:16]
}

func (s *Store) SaveContentTruncation(content string) string {
	hash := contentHash(content)
	runes := []rune(content)
	if len(runes) > 200 {
		runes = runes[:200]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purgeExpiredLocked(now)
	s.enforceLimitLocked()
	s.content[hash] = timedContent{
		info:      ContentTruncationInfo{MessageHash: hash, ContentPreview: string(runes), Timestamp: now},
		expiresAt: s.expiry(now),
	}
	return hash
}

// GetContentTruncation atomically retrieves and removes the entry whose first
// 500 Unicode characters match content.
func (s *Store) GetContentTruncation(content string) (ContentTruncationInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purgeExpiredLocked(now)
	hash := contentHash(content)
	entry, ok := s.content[hash]
	if ok {
		delete(s.content, hash)
	}
	return entry.info, ok
}

func (s *Store) Stats() CacheStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(s.now())
	return CacheStats{ToolTruncations: len(s.tools), ContentTruncations: len(s.content), Total: len(s.tools) + len(s.content)}
}

func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.tools)
	clear(s.content)
}

var defaultStore = newBoundedStore(30*time.Minute, 1024)

func SaveToolTruncation(toolCallID, toolName string, info map[string]any) {
	defaultStore.SaveToolTruncation(toolCallID, toolName, info)
}
func GetToolTruncation(toolCallID string) (ToolTruncationInfo, bool) {
	return defaultStore.GetToolTruncation(toolCallID)
}
func SaveContentTruncation(content string) string { return defaultStore.SaveContentTruncation(content) }
func GetContentTruncation(content string) (ContentTruncationInfo, bool) {
	return defaultStore.GetContentTruncation(content)
}
func GetCacheStats() CacheStats { return defaultStore.Stats() }

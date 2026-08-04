// Kiro Gateway
// Copyright (C) 2025 Jwadow
// SPDX-License-Identifier: AGPL-3.0-or-later

package model

import (
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	contextSuffix = regexp.MustCompile(`(?i)\[\d+[mk]\]$`)
	standard      = regexp.MustCompile(`(?i)^(claude-(?:haiku|sonnet|opus)-\d+)-(\d{1,2})(?:-(?:\d{8}|latest|\d+))?$`)
	noMinor       = regexp.MustCompile(`(?i)^(claude-(?:haiku|sonnet|opus)-\d+)(?:-\d{8})?$`)
	legacy        = regexp.MustCompile(`(?i)^(claude)-(\d+)-(\d+)-(haiku|sonnet|opus)(?:-(?:\d{8}|latest|\d+))?$`)
	dotDate       = regexp.MustCompile(`(?i)^(claude-(?:\d+\.\d+-)?(?:haiku|sonnet|opus)(?:-\d+\.\d+)?)-\d{8}$`)
	inverted      = regexp.MustCompile(`(?i)^claude-(\d+)\.(\d+)-(haiku|sonnet|opus)-(.+)$`)
)

func Normalize(name string) string {
	if name == "" {
		return ""
	}
	name = contextSuffix.ReplaceAllString(name, "")
	lower := strings.ToLower(name)
	if m := standard.FindStringSubmatch(lower); m != nil {
		return m[1] + "." + m[2]
	}
	if m := noMinor.FindStringSubmatch(lower); m != nil {
		return m[1]
	}
	if m := legacy.FindStringSubmatch(lower); m != nil {
		return m[1] + "-" + m[2] + "." + m[3] + "-" + m[4]
	}
	if m := dotDate.FindStringSubmatch(lower); m != nil {
		return m[1]
	}
	if m := inverted.FindStringSubmatch(lower); m != nil {
		return "claude-" + m[3] + "-" + m[1] + "." + m[2]
	}
	return name
}

type Info struct {
	ModelID     string         `json:"modelId"`
	ModelName   string         `json:"modelName,omitempty"`
	Description string         `json:"description,omitempty"`
	TokenLimits map[string]any `json:"tokenLimits,omitempty"`
}
type Cache struct {
	mu            sync.RWMutex
	values        map[string]Info
	updated       time.Time
	ttl           time.Duration
	defaultTokens int
}

func NewCache(ttl time.Duration, defaultTokens int) *Cache {
	return &Cache{values: map[string]Info{}, ttl: ttl, defaultTokens: defaultTokens}
}
func (c *Cache) Update(models []Info) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values = map[string]Info{}
	for _, m := range models {
		c.values[m.ModelID] = m
	}
	c.updated = time.Now()
}
func (c *Cache) Valid(id string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.values[id]
	return ok
}
func (c *Cache) Stale() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.updated.IsZero() || time.Since(c.updated) > c.ttl
}
func (c *Cache) IDs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ids := make([]string, 0, len(c.values))
	for id := range c.values {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
func (c *Cache) MaxInputTokens(id string) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if info, ok := c.values[id]; ok {
		if v, ok := info.TokenLimits["maxInputTokens"].(float64); ok && v > 0 {
			return int(v)
		}
		if v, ok := info.TokenLimits["maxInputTokens"].(int); ok && v > 0 {
			return v
		}
	}
	return c.defaultTokens
}

type Resolution struct {
	InternalID, Source, Original, Normalized string
	Verified                                 bool
}
type Resolver struct {
	Cache          *Cache
	Hidden         map[string]string
	Aliases        map[string]string
	HiddenFromList map[string]struct{}
}

func NewResolver(cache *Cache, hidden, aliases map[string]string, hiddenList []string) *Resolver {
	h := map[string]struct{}{}
	for _, v := range hiddenList {
		h[v] = struct{}{}
	}
	return &Resolver{Cache: cache, Hidden: hidden, Aliases: aliases, HiddenFromList: h}
}
func (r *Resolver) Resolve(name string) Resolution {
	resolved := name
	if v, ok := r.Aliases[name]; ok {
		resolved = v
	}
	normalized := Normalize(resolved)
	if r.Cache.Valid(normalized) {
		return Resolution{normalized, "cache", name, normalized, true}
	}
	if id, ok := r.Hidden[normalized]; ok {
		return Resolution{id, "hidden", name, normalized, true}
	}
	return Resolution{normalized, "passthrough", name, normalized, false}
}

var familyRE = regexp.MustCompile(`(?i)(haiku|sonnet|opus)`)

func ExtractFamily(name string) string {
	if m := familyRE.FindStringSubmatch(name); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

func (r *Resolver) Suggestions(modelName string) []string {
	family := ExtractFamily(modelName)
	if family == "" {
		return r.Available()
	}
	all := r.Available()
	filtered := make([]string, 0, len(all))
	for _, m := range all {
		if strings.Contains(strings.ToLower(m), family) {
			filtered = append(filtered, m)
		}
	}
	return filtered
}

func (r *Resolver) Available() []string {
	set := map[string]struct{}{}
	for _, id := range r.Cache.IDs() {
		set[id] = struct{}{}
	}
	for id := range r.Hidden {
		set[id] = struct{}{}
	}
	for id := range r.HiddenFromList {
		delete(set, id)
	}
	for id := range r.Aliases {
		set[id] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

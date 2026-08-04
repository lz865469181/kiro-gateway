// Kiro Gateway
// Copyright (C) 2025 Jwadow
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Auth interface {
	AccessToken(context.Context) (string, error)
	ForceRefresh(context.Context) (string, error)
	QHost() string
}

type Client struct {
	Auth Auth
	HTTP *http.Client
}

type Response struct {
	ToolUseID string
	Results   map[string]any
}

type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("MCP API returned %d: %s", e.Status, e.Body)
}

func (c *Client) WebSearch(ctx context.Context, query string) (Response, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return Response{}, errors.New("web search query is empty")
	}
	if c.Auth == nil {
		return Response{}, errors.New("MCP authentication is unavailable")
	}
	token, err := c.Auth.AccessToken(ctx)
	if err != nil {
		return Response{}, fmt.Errorf("get MCP access token: %w", err)
	}
	requestID := fmt.Sprintf("web_search_tooluse_%s_%d_%s", random(22), time.Now().UnixMilli(), random(8))
	payload := map[string]any{"id": requestID, "jsonrpc": "2.0", "method": "tools/call", "params": map[string]any{"name": "web_search", "arguments": map[string]any{"query": query}}}
	data, err := json.Marshal(payload)
	if err != nil {
		return Response{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.Auth.QHost(), "/")+"/mcp", bytes.NewReader(data))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-amzn-codewhisperer-optout", "false")
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("call MCP API: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return Response{}, err
	}
	if res.StatusCode != http.StatusOK {
		return Response{}, &HTTPError{Status: res.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	var envelope struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err = json.Unmarshal(body, &envelope); err != nil {
		return Response{}, fmt.Errorf("decode MCP response: %w", err)
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return Response{}, fmt.Errorf("MCP API error: %s", envelope.Error)
	}
	if envelope.Result.IsError {
		return Response{}, errors.New("MCP web search returned an error result")
	}
	if len(envelope.Result.Content) == 0 || strings.TrimSpace(envelope.Result.Content[0].Text) == "" {
		return Response{}, errors.New("MCP API returned no content")
	}
	var results map[string]any
	if err = json.Unmarshal([]byte(envelope.Result.Content[0].Text), &results); err != nil {
		return Response{}, fmt.Errorf("decode MCP result content: %w", err)
	}
	return Response{ToolUseID: "srvtoolu_" + random(32), Results: results}, nil
}

func Summary(query string, results map[string]any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n<web_search>\nSearch results for %q:\n\n", query)
	items, present := results["results"].([]any)
	if !present {
		b.WriteString("No results found.\n")
	} else {
		for i, item := range items {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			title := stringValue(entry["title"], "Untitled")
			fmt.Fprintf(&b, "%d. Title: **%s**\n", i+1, title)
			if published, ok := numberValue(entry["publishedDate"]); ok && published > 0 {
				fmt.Fprintf(&b, "   Published: %s\n", time.UnixMilli(int64(published)).Format("02 Jan 2006 15:04:05"))
			}
			if url := stringValue(entry["url"], ""); url != "" {
				b.WriteString("   URL: " + url + "\n")
			}
			if snippet := stringValue(entry["snippet"], ""); snippet != "" {
				b.WriteString("   " + snippet + "\n")
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("</web_search>\n")
	return b.String()
}

func ResultContent(results map[string]any) []any {
	items, _ := results["results"].([]any)
	content := make([]any, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		content = append(content, map[string]any{
			"type":              "web_search_result",
			"title":             stringValue(entry["title"], ""),
			"url":               stringValue(entry["url"], ""),
			"encrypted_content": stringValue(entry["snippet"], ""),
			"page_age":          nil,
		})
	}
	return content
}

func stringValue(v any, fallback string) string {
	if value, ok := v.(string); ok && value != "" {
		return value
	}
	return fallback
}

func numberValue(v any) (float64, bool) {
	switch value := v.(type) {
	case float64:
		return value, true
	case json.Number:
		n, err := value.Float64()
		return n, err == nil
	default:
		return 0, false
	}
}

func random(n int) string {
	raw := make([]byte, (n+1)/2)
	if _, err := rand.Read(raw); err != nil {
		return strings.Repeat("0", n)
	}
	return hex.EncodeToString(raw)[:n]
}

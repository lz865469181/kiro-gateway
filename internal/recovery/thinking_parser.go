// Kiro Gateway
// Copyright (C) 2025 Jwadow
// SPDX-License-Identifier: AGPL-3.0-or-later

package recovery

import (
	"strings"
	"unicode"
)

// ParserState is the state of the thinking-block finite state machine.
type ParserState int

const (
	PreContent ParserState = iota
	InThinking
	Streaming

	// Explicit aliases are convenient at call sites that also use other FSMs.
	ParserStatePreContent = PreContent
	ParserStateInThinking = InThinking
	ParserStateStreaming  = Streaming
)

const (
	HandlingAsReasoningContent = "as_reasoning_content"
	HandlingRemove             = "remove"
	HandlingPass               = "pass"
	HandlingStripTags          = "strip_tags"
	DefaultInitialBufferSize   = 20
)

var DefaultOpenTags = []string{"<thinking>", "<think>", "<reasoning>", "<thought>"}

// ThinkingParseResult is the output produced for one input chunk. Empty content
// represents Python's None; the FSM never emits a meaningful empty chunk.
type ThinkingParseResult struct {
	ThinkingContent      string
	RegularContent       string
	IsFirstThinkingChunk bool
	IsLastThinkingChunk  bool
	StateChanged         bool
}

type ThinkingParserOption func(*ThinkingParser)

func WithHandlingMode(mode string) ThinkingParserOption {
	return func(p *ThinkingParser) {
		if mode != "" {
			p.HandlingMode = mode
		}
	}
}

func WithOpenTags(tags []string) ThinkingParserOption {
	return func(p *ThinkingParser) {
		if len(tags) != 0 {
			p.OpenTags = append([]string(nil), tags...)
		}
	}
}

func WithInitialBufferSize(size int) ThinkingParserOption {
	return func(p *ThinkingParser) { p.InitialBufferSize = size }
}

// ThinkingParser detects an opening tag only at the beginning of a response.
// It deliberately retains a suffix while thinking so a split closing tag is
// never emitted as thinking content.
type ThinkingParser struct {
	HandlingMode      string
	OpenTags          []string
	InitialBufferSize int
	MaxTagLength      int
	State             ParserState
	InitialBuffer     string
	ThinkingBuffer    string
	OpenTag           string
	CloseTag          string
	FirstThinking     bool
	foundThinking     bool
}

func NewThinkingParser(options ...ThinkingParserOption) *ThinkingParser {
	p := &ThinkingParser{
		HandlingMode:      HandlingAsReasoningContent,
		OpenTags:          append([]string(nil), DefaultOpenTags...),
		InitialBufferSize: DefaultInitialBufferSize,
	}
	for _, option := range options {
		option(p)
	}
	p.recalculateMaxTagLength()
	p.Reset()
	return p
}

func (p *ThinkingParser) recalculateMaxTagLength() {
	p.MaxTagLength = 0
	for _, tag := range p.OpenTags {
		if n := runeLen(tag) * 2; n > p.MaxTagLength {
			p.MaxTagLength = n
		}
	}
}

func (p *ThinkingParser) Feed(content string) ThinkingParseResult {
	if content == "" {
		return ThinkingParseResult{}
	}

	var result ThinkingParseResult
	if p.State == PreContent {
		result = p.handlePreContent(content)
	}
	if p.State == InThinking && !result.StateChanged {
		result = p.handleInThinking(content)
	}
	if p.State == Streaming && !result.StateChanged {
		result.RegularContent = content
	}
	return result
}

func (p *ThinkingParser) handlePreContent(content string) ThinkingParseResult {
	p.InitialBuffer += content
	stripped := strings.TrimLeftFunc(p.InitialBuffer, unicode.IsSpace)

	for _, tag := range p.OpenTags {
		if !strings.HasPrefix(stripped, tag) {
			continue
		}
		p.State = InThinking
		p.OpenTag = tag
		p.CloseTag = "</" + tag[1:]
		p.foundThinking = true
		p.ThinkingBuffer = stripped[len(tag):]
		p.InitialBuffer = ""
		result := p.processThinkingBuffer()
		result.StateChanged = true
		return result
	}

	for _, tag := range p.OpenTags {
		if strings.HasPrefix(tag, stripped) && runeLen(stripped) < runeLen(tag) {
			return ThinkingParseResult{}
		}
	}

	if runeLen(p.InitialBuffer) > p.InitialBufferSize || !p.couldBeTagPrefix(stripped) {
		result := ThinkingParseResult{RegularContent: p.InitialBuffer, StateChanged: true}
		p.InitialBuffer = ""
		p.State = Streaming
		return result
	}
	return ThinkingParseResult{}
}

func (p *ThinkingParser) couldBeTagPrefix(text string) bool {
	if text == "" {
		return true
	}
	for _, tag := range p.OpenTags {
		if strings.HasPrefix(tag, text) {
			return true
		}
	}
	return false
}

func (p *ThinkingParser) handleInThinking(content string) ThinkingParseResult {
	p.ThinkingBuffer += content
	return p.processThinkingBuffer()
}

func (p *ThinkingParser) processThinkingBuffer() ThinkingParseResult {
	if p.CloseTag == "" {
		return ThinkingParseResult{}
	}
	if index := strings.Index(p.ThinkingBuffer, p.CloseTag); index >= 0 {
		thinking := p.ThinkingBuffer[:index]
		after := p.ThinkingBuffer[index+len(p.CloseTag):]
		result := ThinkingParseResult{IsLastThinkingChunk: true, StateChanged: true}
		if thinking != "" {
			result.ThinkingContent = thinking
			result.IsFirstThinkingChunk = p.FirstThinking
			p.FirstThinking = false
		}
		p.State = Streaming
		p.ThinkingBuffer = ""
		if after != "" {
			result.RegularContent = strings.TrimLeftFunc(after, unicode.IsSpace)
		}
		return result
	}

	bufferRunes := []rune(p.ThinkingBuffer)
	if len(bufferRunes) > p.MaxTagLength {
		cut := len(bufferRunes) - p.MaxTagLength
		result := ThinkingParseResult{
			ThinkingContent:      string(bufferRunes[:cut]),
			IsFirstThinkingChunk: p.FirstThinking,
		}
		p.ThinkingBuffer = string(bufferRunes[cut:])
		p.FirstThinking = false
		return result
	}
	return ThinkingParseResult{}
}

func (p *ThinkingParser) Finalize() ThinkingParseResult {
	var result ThinkingParseResult
	if p.ThinkingBuffer != "" {
		if p.State == InThinking {
			result.ThinkingContent = p.ThinkingBuffer
			result.IsFirstThinkingChunk = p.FirstThinking
			result.IsLastThinkingChunk = true
		} else {
			result.RegularContent = p.ThinkingBuffer
		}
		p.ThinkingBuffer = ""
	}
	if p.InitialBuffer != "" {
		result.RegularContent += p.InitialBuffer
		p.InitialBuffer = ""
	}
	return result
}

func (p *ThinkingParser) Reset() {
	p.State = PreContent
	p.InitialBuffer = ""
	p.ThinkingBuffer = ""
	p.OpenTag = ""
	p.CloseTag = ""
	p.FirstThinking = true
	p.foundThinking = false
}

func (p *ThinkingParser) FoundThinkingBlock() bool { return p.foundThinking }

// ProcessForOutput applies the configured handling mode. The bool return is
// false for removed or empty content, mirroring Python's None return.
func (p *ThinkingParser) ProcessForOutput(content string, first, last bool) (string, bool) {
	if content == "" || p.HandlingMode == HandlingRemove {
		return "", false
	}
	if p.HandlingMode == HandlingPass {
		var prefix, suffix string
		if first {
			prefix = p.OpenTag
		}
		if last {
			suffix = p.CloseTag
		}
		return prefix + content + suffix, true
	}
	return content, true
}

func runeLen(s string) int { return len([]rune(s)) }

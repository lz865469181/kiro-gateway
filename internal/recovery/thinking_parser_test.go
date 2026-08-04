package recovery

import "testing"

func TestThinkingParserStatesAndSplitTags(t *testing.T) {
	if PreContent != 0 || InThinking != 1 || Streaming != 2 {
		t.Fatal("parser state values changed")
	}
	p := NewThinkingParser()
	if got := p.Feed("  \n<think"); got != (ThinkingParseResult{}) || p.State != PreContent {
		t.Fatalf("partial opening tag: %#v, state %v", got, p.State)
	}
	got := p.Feed("ing>reasoning split </think")
	if p.State != InThinking || !got.StateChanged || !p.FoundThinkingBlock() {
		t.Fatalf("opening transition: %#v, state %v", got, p.State)
	}
	got = p.Feed("ing>\n\nanswer")
	if p.State != Streaming || !got.IsLastThinkingChunk || got.RegularContent != "answer" {
		t.Fatalf("closing transition: %#v, state %v", got, p.State)
	}
	if got = p.Feed(" <thinking>regular</thinking>"); got.RegularContent != " <thinking>regular</thinking>" {
		t.Fatalf("streaming pass-through: %#v", got)
	}
}

func TestThinkingParserOpeningTagsAndStartOnly(t *testing.T) {
	for _, tag := range DefaultOpenTags {
		p := NewThinkingParser()
		got := p.Feed(tag + "why" + "</" + tag[1:] + "done")
		if !p.FoundThinkingBlock() || p.OpenTag != tag || got.ThinkingContent != "why" || got.RegularContent != "done" {
			t.Errorf("tag %q: parser %#v, result %#v", tag, p, got)
		}
	}
	p := NewThinkingParser()
	got := p.Feed("before <thinking>why</thinking>")
	if p.FoundThinkingBlock() || got.RegularContent != "before <thinking>why</thinking>" {
		t.Fatalf("middle tag detected: %#v", got)
	}
}

func TestThinkingParserCautiousBufferFinalizeAndReset(t *testing.T) {
	p := NewThinkingParser(WithOpenTags([]string{"<t>"}))
	p.Feed("<t>")
	got := p.Feed("abcdefghijklmnopqrstuvwxyz")
	if got.ThinkingContent != "abcdefghijklmnopqrst" || p.ThinkingBuffer != "uvwxyz" || !got.IsFirstThinkingChunk {
		t.Fatalf("cautious result: %#v buffer %q", got, p.ThinkingBuffer)
	}
	got = p.Finalize()
	if got.ThinkingContent != "uvwxyz" || got.IsFirstThinkingChunk || !got.IsLastThinkingChunk {
		t.Fatalf("final result: %#v", got)
	}
	p.Reset()
	if p.State != PreContent || p.FoundThinkingBlock() || !p.FirstThinking || p.OpenTag != "" {
		t.Fatalf("reset failed: %#v", p)
	}

	p = NewThinkingParser()
	p.Feed("<thi")
	if got = p.Finalize(); got.RegularContent != "<thi" {
		t.Fatalf("partial finalize: %#v", got)
	}
}

func TestThinkingParserUnicodeAndMalformedClose(t *testing.T) {
	p := NewThinkingParser()
	got := p.Feed("<thinking>Думаю 🤔</thinking>Ответ")
	if got.ThinkingContent != "Думаю 🤔" || got.RegularContent != "Ответ" {
		t.Fatalf("unicode: %#v", got)
	}
	p = NewThinkingParser()
	p.Feed("<thinking>content")
	p.Feed("</THINKING>")
	if p.State != InThinking {
		t.Fatal("case-insensitive close unexpectedly accepted")
	}
}

func TestThinkingParserOutputModes(t *testing.T) {
	p := NewThinkingParser(WithHandlingMode(HandlingPass))
	p.OpenTag, p.CloseTag = "<thinking>", "</thinking>"
	if got, ok := p.ProcessForOutput("content", true, true); !ok || got != "<thinking>content</thinking>" {
		t.Fatalf("pass: %q %v", got, ok)
	}
	p.HandlingMode = HandlingRemove
	if _, ok := p.ProcessForOutput("content", true, true); ok {
		t.Fatal("remove returned content")
	}
	p.HandlingMode = HandlingStripTags
	if got, ok := p.ProcessForOutput("content", true, true); !ok || got != "content" {
		t.Fatalf("strip: %q %v", got, ok)
	}
	if _, ok := p.ProcessForOutput("", true, true); ok {
		t.Fatal("empty content returned")
	}
}

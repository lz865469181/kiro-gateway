package protocol

import (
	"testing"

	"github.com/jwadow/kiro-gateway-go/internal/recovery"
)

func TestMatchingBrace(t *testing.T) {
	cases := []struct {
		in          string
		start, want int
	}{{`{"key":"value"}`, 0, 14}, {`{"a":{"b":1}}`, 0, 12}, {`{"a":"{}"}`, 0, 9}, {`{"a":1`, 0, -1}, {`x{"a":1}`, 0, -1}}
	for _, tc := range cases {
		if got := MatchingBrace(tc.in, tc.start); got != tc.want {
			t.Errorf("MatchingBrace(%q)=%d want %d", tc.in, got, tc.want)
		}
	}
}
func TestParserChunksAndTools(t *testing.T) {
	p := NewParser()
	if events := p.Feed([]byte(`garbage{"content":"Hel`)); len(events) != 0 {
		t.Fatal(events)
	}
	events := p.Feed([]byte(`lo"}{"usage":1.5}{"name":"read","toolUseId":"call_1"}{"input":"{\"path\":\"a\"}"}{"stop":true}`))
	if len(events) != 2 || events[0].Content != "Hello" {
		t.Fatalf("events=%v", events)
	}
	calls := p.ToolCalls()
	if len(calls) != 1 || calls[0].Function.Arguments != `{"path":"a"}` {
		t.Fatalf("calls=%v", calls)
	}
}
func TestParserDeduplicates(t *testing.T) {
	calls := Deduplicate([]ToolCall{{ID: "1", Function: ToolFunction{Name: "f", Arguments: "{}"}}, {ID: "1", Function: ToolFunction{Name: "f", Arguments: `{"x":1}`}}, {ID: "2", Function: ToolFunction{Name: "f", Arguments: `{"x":1}`}}})
	if len(calls) != 1 || calls[0].ID != "1" {
		t.Fatalf("calls=%v", calls)
	}
}
func TestTruncatedToolStoredForRecovery(t *testing.T) {
	p := NewParser()
	p.Feed([]byte(`{"name":"write","toolUseId":"truncated_1"}{"input":"{\"path\":\"x"}{"stop":true}`))
	calls := p.ToolCalls()
	if len(calls) != 1 || !calls[0].Truncated {
		t.Fatalf("calls=%+v", calls)
	}
	info, ok := recovery.GetToolTruncation("truncated_1")
	if !ok || info.ToolName != "write" {
		t.Fatalf("info=%+v ok=%v", info, ok)
	}
}

func TestBracketCalls(t *testing.T) {
	calls := ParseBracketToolCalls(`[Called run with args: {"command":{"name":"test"}}]`)
	if len(calls) != 1 || calls[0].Function.Name != "run" {
		t.Fatalf("calls=%v", calls)
	}
}

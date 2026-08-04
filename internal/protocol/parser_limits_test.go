package protocol

import (
	"bytes"
	"testing"
)

func TestParserRejectsOversizedIncompleteEvent(t *testing.T) {
	p := NewParser()
	chunk := append([]byte(`{"content":"`), bytes.Repeat([]byte("x"), MaxBufferBytes)...)
	if events := p.Feed(chunk); len(events) != 0 {
		t.Fatalf("events=%v", events)
	}
	if p.Err() == nil {
		t.Fatal("expected bounded buffer error")
	}
}

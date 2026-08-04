package model

import "testing"

func TestNormalize(t *testing.T) {
	cases := map[string]string{"claude-haiku-4-5-20251001": "claude-haiku-4.5", "claude-sonnet-4-20250514": "claude-sonnet-4", "claude-3-7-sonnet": "claude-3.7-sonnet", "claude-4.5-opus-high": "claude-opus-4.5", "claude-sonnet-4.5[1m]": "claude-sonnet-4.5", "auto": "auto"}
	for input, want := range cases {
		if got := Normalize(input); got != want {
			t.Errorf("Normalize(%q)=%q want %q", input, got, want)
		}
	}
}

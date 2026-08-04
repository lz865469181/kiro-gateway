package accounts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestExpandCredentials(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "folder")
	if err := os.Mkdir(folder, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) string {
		path := filepath.Join(folder, name)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	first := write("b.json", `{"refreshToken":"b"}`)
	second := write("a.json", `{"clientId":"a"}`)
	write("invalid.json", `{`)
	write("notes.txt", `not credentials`)

	configPath := filepath.Join(dir, "credentials.json")
	entries := []map[string]any{
		{"type": "json", "path": folder},
		{"type": "refresh_token", "refresh_token": "secret", "region": "eu-west-1"},
		{"type": "refresh_token", "refresh_token": "secret"}, // duplicate stable ID
		{"type": "json", "path": first, "enabled": false},
		{"type": "refresh_token"},
	}
	data, _ := json.Marshal(entries)
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	got, err := ExpandCredentials(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d credentials: %#v", len(got), got)
	}
	second, _ = filepath.Abs(second)
	first, _ = filepath.Abs(first)
	if got[0].ID != second || got[1].ID != first {
		t.Fatalf("folder order = [%q, %q], want [%q, %q]", got[0].ID, got[1].ID, second, first)
	}
	if got[2].ID != "refresh_token_2bb80d537b1da3e3" || got[2].Region != "eu-west-1" {
		t.Fatalf("refresh credential = %#v", got[2])
	}
}

func TestExpandCredentialsMissingFile(t *testing.T) {
	got, err := ExpandCredentials(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil || len(got) != 0 {
		t.Fatalf("got=%v err=%v", got, err)
	}
}

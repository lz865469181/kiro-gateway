package debuglog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestErrorsModeFlushesOnlyOnError(t *testing.T) {
	dir := t.TempDir()
	logger := &Logger{Mode: Errors, Dir: dir}
	logger.Prepare()
	logger.Request([]byte(`{"x":1}`))
	logger.Raw([]byte("raw"))
	if _, err := os.Stat(filepath.Join(dir, "request_body.json")); !os.IsNotExist(err) {
		t.Fatalf("written before flush: %v", err)
	}
	logger.FlushError(500, "failed")
	for _, name := range []string{"request_body.json", "raw_response.bin", "error_info.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
func TestAllModeWritesImmediately(t *testing.T) {
	dir := t.TempDir()
	logger := &Logger{Mode: All, Dir: dir}
	logger.Prepare()
	logger.Modified([]byte("chunk"))
	data, err := os.ReadFile(filepath.Join(dir, "modified_response.txt"))
	if err != nil || string(data) != "chunk" {
		t.Fatalf("data=%q err=%v", data, err)
	}
}

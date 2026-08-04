// Kiro Gateway
// Copyright (C) 2025 Jwadow
// SPDX-License-Identifier: AGPL-3.0-or-later

package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteFileReplacesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("content=%q", data)
	}
	if _, err := os.Stat(path + ".backup"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup remains: %v", err)
	}
}

func TestReplaceUsesBackupWhenDirectReplacementFails(t *testing.T) {
	files := map[string]string{"tmp": "new", "dst": "old"}
	installAttempts := 0
	rename := func(old, new string) error {
		if old == "tmp" && new == "dst" {
			installAttempts++
			if installAttempts == 1 {
				return errors.New("destination exists")
			}
		}
		value, ok := files[old]
		if !ok {
			return os.ErrNotExist
		}
		delete(files, old)
		files[new] = value
		return nil
	}
	remove := func(path string) error {
		if _, ok := files[path]; !ok {
			return os.ErrNotExist
		}
		delete(files, path)
		return nil
	}
	stat := func(path string) (os.FileInfo, error) {
		if _, ok := files[path]; !ok {
			return nil, os.ErrNotExist
		}
		return fakeFileInfo{}, nil
	}

	if err := replace("tmp", "dst", rename, remove, stat); err != nil {
		t.Fatal(err)
	}
	if files["dst"] != "new" || installAttempts != 2 {
		t.Fatalf("replacement not installed: attempts=%d files=%#v", installAttempts, files)
	}
	if _, ok := files["dst.backup"]; ok {
		t.Fatalf("backup remains: %#v", files)
	}
}

func TestReplaceRestoresDestinationWhenInstallFails(t *testing.T) {
	files := map[string]string{"tmp": "new", "dst": "old"}
	installAttempts := 0
	rename := func(old, new string) error {
		if old == "tmp" && new == "dst" {
			installAttempts++
			if installAttempts <= 2 {
				return errors.New("injected install failure")
			}
		}
		value, ok := files[old]
		if !ok {
			return os.ErrNotExist
		}
		delete(files, old)
		files[new] = value
		return nil
	}
	remove := func(path string) error {
		if _, ok := files[path]; !ok {
			return os.ErrNotExist
		}
		delete(files, path)
		return nil
	}
	stat := func(path string) (os.FileInfo, error) {
		if _, ok := files[path]; !ok {
			return nil, os.ErrNotExist
		}
		return fakeFileInfo{}, nil
	}

	if err := replace("tmp", "dst", rename, remove, stat); err == nil {
		t.Fatal("expected install failure")
	}
	if files["dst"] != "old" {
		t.Fatalf("destination not restored: %#v", files)
	}
	if files["tmp"] != "new" {
		t.Fatalf("replacement unexpectedly lost: %#v", files)
	}
	if _, ok := files["dst.backup"]; ok {
		t.Fatalf("backup remains after restoration: %#v", files)
	}
}

type fakeFileInfo struct{}

func (fakeFileInfo) Name() string       { return "file" }
func (fakeFileInfo) Size() int64        { return 0 }
func (fakeFileInfo) Mode() os.FileMode  { return 0600 }
func (fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fakeFileInfo) IsDir() bool        { return false }
func (fakeFileInfo) Sys() any           { return nil }

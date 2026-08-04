// Kiro Gateway
// Copyright (C) 2025 Jwadow
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package atomicfile writes files without exposing partial contents or losing the
// previous version when a platform cannot replace an existing file directly.
package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WriteFile atomically installs data at path. If installing the new file fails
// after the old file has been moved aside, the old file is restored.
func WriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".atomic-write-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err = file.Chmod(mode); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return replace(tmp, path, os.Rename, os.Remove, os.Stat)
}

type renameFunc func(string, string) error
type removeFunc func(string) error
type statFunc func(string) (os.FileInfo, error)

func replace(tmp, path string, rename renameFunc, remove removeFunc, stat statFunc) error {
	installErr := rename(tmp, path)
	if installErr == nil {
		return nil
	}
	if _, err := stat(path); err != nil {
		return installErr
	}

	backup := path + ".backup"
	if err := remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("prepare atomic-write backup: %w", err)
	}
	if err := rename(path, backup); err != nil {
		return fmt.Errorf("preserve existing file: %w", err)
	}
	if err := rename(tmp, path); err != nil {
		if restoreErr := rename(backup, path); restoreErr != nil {
			return errors.Join(fmt.Errorf("install replacement: %w", err), fmt.Errorf("restore existing file: %w", restoreErr))
		}
		return fmt.Errorf("install replacement: %w", err)
	}
	if err := remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove atomic-write backup: %w", err)
	}
	return nil
}

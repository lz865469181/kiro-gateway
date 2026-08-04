// Kiro Gateway
// Copyright (C) 2025 Jwadow
// SPDX-License-Identifier: AGPL-3.0-or-later

package accounts

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	_ "modernc.org/sqlite"
)

// ExpandCredentials reads the account configuration, expands directories,
// validates candidate files, and returns accounts in deterministic order.
func ExpandCredentials(path string) ([]Credential, error) {
	path, err := expandPath(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read credentials config: %w", err)
	}
	var entries []Credential
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("decode credentials config: %w", err)
	}

	out := make([]Credential, 0, len(entries))
	seen := make(map[string]struct{})
	for _, entry := range entries {
		if !entry.isEnabled() {
			continue
		}
		if entry.Region == "" {
			entry.Region = "us-east-1"
		}
		switch entry.Type {
		case RefreshTokenCredential:
			if entry.RefreshToken == "" {
				continue
			}
			sum := sha256.Sum256([]byte(entry.RefreshToken))
			entry.ID = "refresh_token_" + hex.EncodeToString(sum[:8])
			out = appendCredential(out, seen, entry)
		case JSONCredential, SQLiteCredential:
			if entry.Path == "" {
				continue
			}
			expanded, expandErr := expandPath(entry.Path)
			if expandErr != nil {
				continue
			}
			info, statErr := os.Stat(expanded)
			if statErr != nil {
				continue
			}
			if !info.IsDir() {
				entry.Path, entry.ID = expanded, expanded
				out = appendCredential(out, seen, entry)
				continue
			}
			children, readErr := os.ReadDir(expanded)
			if readErr != nil {
				continue
			}
			sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
			for _, child := range children {
				if child.IsDir() {
					continue
				}
				candidate := filepath.Join(expanded, child.Name())
				if !validCredentialFile(entry.Type, candidate) {
					continue
				}
				expandedEntry := entry
				expandedEntry.Path, expandedEntry.ID = candidate, candidate
				out = appendCredential(out, seen, expandedEntry)
			}
		}
	}
	return out, nil
}

func appendCredential(out []Credential, seen map[string]struct{}, c Credential) []Credential {
	if _, exists := seen[c.ID]; exists {
		return out
	}
	seen[c.ID] = struct{}{}
	return append(out, c)
}

func validCredentialFile(kind CredentialType, path string) bool {
	switch kind {
	case JSONCredential:
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		var value map[string]json.RawMessage
		if json.Unmarshal(data, &value) != nil {
			return false
		}
		_, refresh := value["refreshToken"]
		_, client := value["clientId"]
		return refresh || client
	case SQLiteCredential:
		db, err := sql.Open("sqlite", path+"?mode=ro")
		if err != nil {
			return false
		}
		defer db.Close()
		var name string
		return db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='auth_kv'").Scan(&name) == nil
	default:
		return false
	}
}

func expandPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if path == "~" || len(path) > 2 && path[0] == '~' && (path[1] == '/' || path[1] == '\\') {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, path[2:])
		}
	}
	return filepath.Abs(path)
}

// Kiro Gateway
// Copyright (C) 2025 Jwadow
// SPDX-License-Identifier: AGPL-3.0-or-later

package debuglog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type Mode string

const (
	Off    Mode = "off"
	Errors Mode = "errors"
	All    Mode = "all"
)

type Logger struct {
	mu                           sync.Mutex
	Mode                         Mode
	Dir                          string
	request, kiro, raw, modified []byte
}

func (l *Logger) Prepare() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.request = nil
	l.kiro = nil
	l.raw = nil
	l.modified = nil
	if l.Mode == All {
		_ = os.RemoveAll(l.Dir)
		_ = os.MkdirAll(l.Dir, 0755)
	}
}
func (l *Logger) Request(data []byte)     { l.store("request_body.json", data, &l.request, false) }
func (l *Logger) KiroRequest(data []byte) { l.store("kiro_request_body.json", data, &l.kiro, false) }
func (l *Logger) Raw(data []byte)         { l.store("raw_response.bin", data, &l.raw, true) }
func (l *Logger) Modified(data []byte)    { l.store("modified_response.txt", data, &l.modified, true) }
func (l *Logger) store(name string, data []byte, buffer *[]byte, appendFile bool) {
	if l.Mode == Off {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Mode == Errors {
		*buffer = append(*buffer, data...)
		return
	}
	_ = os.MkdirAll(l.Dir, 0755)
	flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendFile {
		flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	if f, err := os.OpenFile(filepath.Join(l.Dir, name), flag, 0600); err == nil {
		_, _ = f.Write(data)
		_ = f.Close()
	}
}
func (l *Logger) FlushError(status int, message string) {
	if l.Mode == Off {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = os.MkdirAll(l.Dir, 0755)
	write := func(name string, data []byte) {
		if len(data) > 0 {
			_ = os.WriteFile(filepath.Join(l.Dir, name), data, 0600)
		}
	}
	if l.Mode == Errors {
		write("request_body.json", l.request)
		write("kiro_request_body.json", l.kiro)
		write("raw_response.bin", l.raw)
		write("modified_response.txt", l.modified)
	}
	data, _ := json.MarshalIndent(map[string]any{"status_code": status, "error_message": message}, "", "  ")
	write("error_info.json", data)
}

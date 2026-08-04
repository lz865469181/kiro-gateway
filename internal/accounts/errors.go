// Kiro Gateway
// Copyright (C) 2025 Jwadow
// SPDX-License-Identifier: AGPL-3.0-or-later

package accounts

// ErrorType controls whether a request should fail over to another account.
type ErrorType string

const (
	Fatal       ErrorType = "fatal"
	Recoverable ErrorType = "recoverable"
)

// ClassifyError classifies request errors using the same conservative rules as
// the Python gateway. Unknown errors are fatal so they are not retried across
// every account.
func ClassifyError(statusCode int, reason string) ErrorType {
	switch statusCode {
	case 402, 403, 429:
		return Recoverable
	case 400:
		if reason == "INVALID_MODEL_ID" {
			return Recoverable
		}
	}
	return Fatal
}

package accounts

import "testing"

func TestClassifyError(t *testing.T) {
	tests := []struct {
		status int
		reason string
		want   ErrorType
	}{
		{400, "INVALID_MODEL_ID", Recoverable},
		{402, "MONTHLY_REQUEST_COUNT", Recoverable},
		{403, "", Recoverable},
		{429, "", Recoverable},
		{400, "CONTENT_LENGTH_EXCEEDS_THRESHOLD", Fatal},
		{400, "", Fatal},
		{401, "", Fatal},
		{422, "", Fatal},
		{503, "", Fatal},
	}
	for _, test := range tests {
		if got := ClassifyError(test.status, test.reason); got != test.want {
			t.Errorf("ClassifyError(%d, %q) = %q, want %q", test.status, test.reason, got, test.want)
		}
	}
}

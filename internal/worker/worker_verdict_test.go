package worker

import "testing"

func TestDeriveWorkerVerdict(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "empty string",
			input: "",
			want:  "unknown",
		},
		{
			name:  "pass output",
			input: "ok  github.com/x/y 0.2s\nPASS\n",
			want:  "pass",
		},
		{
			name:  "fail output",
			input: "--- FAIL: TestFoo\nFAIL\n",
			want:  "fail",
		},
		{
			name:  "mixed output — fail wins",
			input: "ok  github.com/x/y 0.2s\n--- FAIL: TestFoo\n",
			want:  "fail",
		},
		{
			name:  "prose",
			input: "some prose the worker wrote",
			want:  "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveWorkerVerdict(tt.input); got != tt.want {
				t.Errorf("deriveWorkerVerdict(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

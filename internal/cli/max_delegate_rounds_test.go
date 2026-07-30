package cli

import (
	"testing"
)

func TestDefaultMaxDelegateRounds_IsThree(t *testing.T) {
	if defaultMaxDelegateRounds != 3 {
		t.Errorf("defaultMaxDelegateRounds = %d, want 3", defaultMaxDelegateRounds)
	}
}

func TestMaxDelegateRounds_Default(t *testing.T) {
	// Ensure the env var is unset.
	t.Setenv("RIG_MAX_DELEGATE_ROUNDS", "")
	if got := maxDelegateRounds(); got != 3 {
		t.Errorf("maxDelegateRounds() = %d, want 3 (default)", got)
	}
}

func TestMaxDelegateRounds_CustomValue(t *testing.T) {
	t.Setenv("RIG_MAX_DELEGATE_ROUNDS", "7")
	if got := maxDelegateRounds(); got != 7 {
		t.Errorf("maxDelegateRounds() = %d, want 7", got)
	}
}

func TestMaxDelegateRounds_GarbageFallsBack(t *testing.T) {
	t.Setenv("RIG_MAX_DELEGATE_ROUNDS", "abc")
	if got := maxDelegateRounds(); got != 3 {
		t.Errorf("maxDelegateRounds() = %d, want 3 (fallback on garbage)", got)
	}
}

func TestMaxDelegateRounds_ZeroDisables(t *testing.T) {
	t.Setenv("RIG_MAX_DELEGATE_ROUNDS", "0")
	if got := maxDelegateRounds(); got != 0 {
		t.Errorf("maxDelegateRounds() = %d, want 0 (disabled)", got)
	}
}

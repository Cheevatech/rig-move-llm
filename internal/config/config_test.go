package config

import "testing"

// TestCustomSubagentUsageParsing: only 1-99 enables the L4 %-budget; anything
// else — unset, 0, 100, out-of-range, garbage — must land on 100 (all custom),
// because a bad value silently diverting traffic to paid quota would violate
// the "default never burns quota" floor.
func TestCustomSubagentUsageParsing(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate from any real global config.env

	cases := []struct {
		env  string
		want int
	}{
		{"", 100},
		{"40", 40},
		{"1", 1},
		{"99", 99},
		{"0", 100},
		{"100", 100},
		{"150", 100},
		{"-5", 100},
		{"abc", 100},
	}
	for _, c := range cases {
		t.Setenv("CUSTOM_SUBAGENT_USAGE", c.env)
		if got := LoadFrom(t.TempDir()).CustomSubagentUsage; got != c.want {
			t.Errorf("CUSTOM_SUBAGENT_USAGE=%q: got %d, want %d", c.env, got, c.want)
		}
	}
}

package config

import "testing"

func TestHealthConfigDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate the global scope from the dev machine
	c := LoadFrom(t.TempDir())
	if c.WorkerHealthPath != "/v1/models" {
		t.Errorf("default WorkerHealthPath = %q, want /v1/models", c.WorkerHealthPath)
	}
	if c.HealthTimeoutMs != 2000 {
		t.Errorf("default HealthTimeoutMs = %d, want 2000", c.HealthTimeoutMs)
	}
	if c.HealthCacheSec != 15 {
		t.Errorf("default HealthCacheSec = %d, want 15", c.HealthCacheSec)
	}
}

func TestHealthConfigOverrides(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WORKER_HEALTH_PATH", "off")
	t.Setenv("WORKER_HEALTH_TIMEOUT_MS", "500")
	t.Setenv("WORKER_HEALTH_CACHE_SEC", "0")
	c := LoadFrom(t.TempDir())
	if c.WorkerHealthPath != "off" {
		t.Errorf("WorkerHealthPath = %q, want off", c.WorkerHealthPath)
	}
	if c.HealthTimeoutMs != 500 {
		t.Errorf("HealthTimeoutMs = %d, want 500", c.HealthTimeoutMs)
	}
	if c.HealthCacheSec != 0 {
		t.Errorf("HealthCacheSec = %d, want 0", c.HealthCacheSec)
	}
}

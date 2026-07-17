package config

import "strings"

// Backend describes an OpenAI-compatible worker endpoint family and the quirks
// that distinguish it. The translator emits a controlled, minimal OpenAI request
// (see pkg/translate), so a backend only needs to declare a sensible default
// base URL plus the handful of deviations real servers have.
//
// Ollama is first-class: `--backend ollama` (or WORKER_BACKEND=ollama) needs no
// base URL at all — it resolves to the local daemon.
type Backend struct {
	Name string
	// DefaultBase is used when WORKER_API_BASE is not set. Empty means the user
	// MUST provide a base URL (e.g. a bare "openai" backend).
	DefaultBase string
	// DropStreamOptions strips {"stream_options":{"include_usage":true}} from the
	// worker request. We normally request usage so the token accounting is exact;
	// some older servers 400 on the field, so those backends opt out.
	DropStreamOptions bool
	// ExtraHeaders are added verbatim to every worker request (e.g. OpenRouter's
	// optional attribution headers). Never carries secrets.
	ExtraHeaders map[string]string
}

// registry is the set of known backends, keyed by lowercase name.
var registry = map[string]Backend{
	"ollama": {
		Name:        "ollama",
		DefaultBase: "http://localhost:11434/v1",
	},
	"llamacpp": {
		Name:        "llamacpp",
		DefaultBase: "http://localhost:8080/v1",
	},
	"tabby": { // ExLlama via TabbyAPI
		Name:        "tabby",
		DefaultBase: "http://localhost:5000/v1",
	},
	"openrouter": {
		Name:        "openrouter",
		DefaultBase: "https://openrouter.ai/api/v1",
		ExtraHeaders: map[string]string{
			"HTTP-Referer": "https://github.com/rigmovellm/rig-move-llm",
			"X-Title":      "rig-move-llm",
		},
	},
	"openai": {
		Name:        "openai",
		DefaultBase: "https://api.openai.com/v1",
	},
	// generic = any OpenAI-compatible endpoint; no assumptions, base URL required.
	"generic": {
		Name: "generic",
	},
}

// LookupBackend returns the backend for name (case-insensitive). Unknown or empty
// names fall back to "generic", so a user can always just point at a base URL.
func LookupBackend(name string) Backend {
	if b, ok := registry[strings.ToLower(strings.TrimSpace(name))]; ok {
		return b
	}
	return registry["generic"]
}

// BackendNames returns the known backend names (for CLI help / init prompts).
func BackendNames() []string {
	return []string{"ollama", "llamacpp", "tabby", "openrouter", "openai", "generic"}
}

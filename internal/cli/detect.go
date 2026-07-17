package cli

import (
	"encoding/json"
	"net/http"
	"time"
)

// detected is the outcome of probing the machine for a local worker endpoint.
type detected struct {
	Backend string
	Base    string
	Model   string // best-effort first model, if the backend exposes a list
}

// detectWorker probes the well-known local endpoints (Ollama, then llama.cpp) and
// returns the first that answers. Zero value means nothing was found. Short
// timeouts keep `init` snappy on machines with no local model.
func detectWorker() (detected, bool) {
	if d, ok := detectOllama(); ok {
		return d, true
	}
	if d, ok := detectLlamaCpp(); ok {
		return d, true
	}
	return detected{}, false
}

var probeClient = &http.Client{Timeout: 500 * time.Millisecond}

// detectOllama hits Ollama's native /api/tags (specific, not shared with other
// servers on 11434) and picks the first installed model.
func detectOllama() (detected, bool) {
	resp, err := probeClient.Get("http://localhost:11434/api/tags")
	if err != nil || resp.StatusCode != http.StatusOK {
		return detected{}, false
	}
	defer resp.Body.Close()
	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	d := detected{Backend: "ollama", Base: "http://localhost:11434/v1"}
	if len(body.Models) > 0 {
		d.Model = body.Models[0].Name
	}
	return d, true
}

// detectLlamaCpp hits the OpenAI-compatible /v1/models on llama-server's default
// port and picks the first served model.
func detectLlamaCpp() (detected, bool) {
	resp, err := probeClient.Get("http://localhost:8080/v1/models")
	if err != nil || resp.StatusCode != http.StatusOK {
		return detected{}, false
	}
	defer resp.Body.Close()
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	d := detected{Backend: "llamacpp", Base: "http://localhost:8080/v1"}
	if len(body.Data) > 0 {
		d.Model = body.Data[0].ID
	}
	return d, true
}

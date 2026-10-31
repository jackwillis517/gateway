package provider

import "strings"

// NewOllama returns a provider for an Ollama server's OpenAI-compatible
// endpoint. baseURL is the server root, for example http://localhost:11434.
// Ollama needs no API key.
func NewOllama(baseURL string) *OpenAICompatible {
	return newOpenAICompatible("ollama", strings.TrimRight(baseURL, "/")+"/v1", "")
}

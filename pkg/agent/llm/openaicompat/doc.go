// Package openaicompat holds the OpenAI-Chat-Completions wire translation shared
// by every OpenAI-compatible llm.Provider (openai, openrouter). It converts the
// neutral llm.Request/Response types to and from github.com/openai/openai-go/v3
// and runs the streaming accumulate loop. Provider-specific concerns (base URL,
// extra body fields, reported cost) live in the provider packages, not here.
package openaicompat

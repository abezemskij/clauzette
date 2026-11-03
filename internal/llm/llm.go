// Package llm defines what the agent needs from a model server, so the agent
// does not depend on one concrete client.
//
// Backend is the part every provider can offer. Local adds what only a local
// server such as Ollama has (loading a model, GPU placement, model details);
// callers check for it with a type assertion and skip those features when it
// is missing. Only Ollama implements them.
//
// The message types live in this package (types.go); package ollama keeps
// aliases for them. Their JSON field names must not change: saved sessions
// embed them.
package llm

import (
	"context"
	"time"
)

// Backend streams chat replies and lists the available models.
type Backend interface {
	ChatStream(ctx context.Context, req ChatRequest, onDelta func(Message)) (*Result, error)
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// Local is a Backend that runs models on this cluster's own hardware.
type Local interface {
	Backend
	BaseURL() string
	Version(ctx context.Context) (string, error)
	Load(ctx context.Context, model string, options map[string]any, keepAlive string) (time.Duration, error)
	Unload(ctx context.Context, model string) error
	PS(ctx context.Context) ([]RunningModel, error)
	ShowModelContext(ctx context.Context, model string) (int, error)
	ShowWeights(ctx context.Context, model string) (string, error)
}

package llm

import (
	"context"
	"errors"
	"sync"
	"time"
)

// The provider-neutral message types. They began in package ollama (whose
// API shape they follow, the same shape as OpenAI's), which keeps aliases
// for them. Saved sessions embed Message as JSON, so the JSON field names
// must not change.

// ErrStalled is returned when the model sends nothing for longer than the
// timeout of the current phase (before the first token, or between tokens).
var ErrStalled = errors.New("no data from the model within the timeout")

type ToolCallFunction struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// ToolCall is one tool call from the model. ID links it to its result
// (Message.ToolCallID): Ollama does not need it, but other providers do, so
// the agent gives every call one and keeps it in the session.
type ToolCall struct {
	ID       string           `json:"id,omitempty"`
	Function ToolCallFunction `json:"function"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Thinking   string     `json:"thinking,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"` // on a tool result: the ToolCall.ID it answers

	// ImageRefs are the images attached to this message (user messages
	// only). The session stores these references, never the image data.
	ImageRefs []Image `json:"image_refs,omitempty"`
	// Images is the data of ImageRefs, base64-encoded, in the same order.
	// It is filled in on the copy of a message that goes into a request
	// (Ollama reads this field as it is) and is never saved.
	Images []string `json:"images,omitempty"`
}

// Image is an image attached to a message. The data is kept by the
// agent's image store under its SHA-256, so a session refers to the exact
// bytes the model saw even if the original file changes or is deleted.
type Image struct {
	Path   string `json:"path"` // where it came from, for display
	MIME   string `json:"mime"` // image/png, image/jpeg, image/webp, image/gif
	SHA256 string `json:"sha256"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Bytes  int64  `json:"bytes"`
}

type FunctionSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type ToolSpec struct {
	Type     string       `json:"type"`
	Function FunctionSpec `json:"function"`
}

type ChatRequest struct {
	Model     string         `json:"model"`
	Messages  []Message      `json:"messages"`
	Tools     []ToolSpec     `json:"tools,omitempty"`
	Stream    bool           `json:"stream"`
	Think     *bool          `json:"think,omitempty"`
	Options   map[string]any `json:"options,omitempty"`
	KeepAlive string         `json:"keep_alive,omitempty"`
}

// Result is a complete (or, on error, partial) assistant response.
type Result struct {
	Message         Message
	DoneReason      string
	PromptEvalCount int
	EvalCount       int
	LoadDuration    time.Duration
	PromptDuration  time.Duration
	EvalDuration    time.Duration
}

func (r *Result) TokensPerSecond() float64 {
	if r == nil || r.EvalDuration <= 0 {
		return 0
	}
	return float64(r.EvalCount) / r.EvalDuration.Seconds()
}

type Timeouts struct {
	Connect    time.Duration // establishing the TCP connection
	FirstToken time.Duration // from sending a request to the first streamed data
	InterToken time.Duration // maximum silence once data is flowing
	Load       time.Duration // explicit model load (warm-up)
}

type RunningModel struct {
	Name          string    `json:"name"`
	Model         string    `json:"model"`
	Size          int64     `json:"size"`
	SizeVRAM      int64     `json:"size_vram"`
	ExpiresAt     time.Time `json:"expires_at"`
	ContextLength int       `json:"context_length"`
}

// Matches reports whether this running model is the configured model.
func (m RunningModel) Matches(model string) bool {
	return m.Name == model || m.Model == model || m.Name == model+":latest"
}

// ModelInfo is one installed model, as /api/tags (the list behind
// `ollama list`) reports it. Newer Ollama versions include the model's
// capabilities and native context length; older ones leave them empty.
type ModelInfo struct {
	Name         string    `json:"name"`
	Model        string    `json:"model"`
	Digest       string    `json:"digest"` // the model (manifest) digest; changes with any Modelfile edit
	Size         int64     `json:"size"`
	ModifiedAt   time.Time `json:"modified_at"`
	Capabilities []string  `json:"capabilities"`
	Details      struct {
		Family            string `json:"family"`
		ParameterSize     string `json:"parameter_size"`
		QuantizationLevel string `json:"quantization_level"`
		ContextLength     int    `json:"context_length"`
	} `json:"details"`
	Pricing *Pricing `json:"-"` // cloud models only; nil = unknown
}

// Pricing is a cloud model's price in US dollars per token. Ollama models
// have none.
type Pricing struct {
	Prompt, Completion float64
}

// Free reports whether the model costs nothing. A model without known
// pricing is not free.
func (m ModelInfo) Free() bool {
	return m.Pricing != nil && m.Pricing.Prompt == 0 && m.Pricing.Completion == 0
}

// Matches reports whether this is the named model.
func (m ModelInfo) Matches(model string) bool {
	return m.Name == model || m.Model == model || m.Name == model+":latest"
}

// Has reports whether the model declares a capability ("tools", "thinking", "vision", ...).
func (m ModelInfo) Has(capability string) bool {
	for _, c := range m.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// Watchdog cancels a request when no data arrives within the current
// phase's timeout. Kick restarts the countdown after each chunk.
type Watchdog struct {
	mu     sync.Mutex
	d      time.Duration
	phase  string
	timer  *time.Timer
	fired  bool
	cancel context.CancelFunc
}

func NewWatchdog(d time.Duration, phase string, cancel context.CancelFunc) *Watchdog {
	w := &Watchdog{cancel: cancel}
	w.Reset(d, phase)
	return w
}

func (w *Watchdog) Reset(d time.Duration, phase string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.d, w.phase = d, phase
	if d > 0 {
		w.timer = time.AfterFunc(d, w.fire)
	}
}

func (w *Watchdog) Kick() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Reset(w.d)
	}
}

func (w *Watchdog) fire() {
	w.mu.Lock()
	w.fired = true
	w.mu.Unlock()
	w.cancel()
}

func (w *Watchdog) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
	}
}

func (w *Watchdog) FiredPhase() (bool, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fired, w.phase
}

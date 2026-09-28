// Package ai owns Boop's two AI features: the cached author status shown in the
// home right rail and the owner-only writing assistant. Both go through one
// OpenAI-compatible chat client with an explicit timeout, a response cap, a
// process-wide concurrency limit and failures that carry a stable code instead
// of a key, a prompt, a model reply or a raw upstream body.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Failure codes. A code is the only thing an AI failure ever carries, so an HTTP
// error, a log line and ai_cache.last_error can all share it without leaking
// anything the model or the operator sent.
const (
	CodeDisabled     = "disabled"
	CodeUnconfigured = "unconfigured"
	CodeTimeout      = "timeout"
	CodeBusy         = "busy"
	CodeUpstream     = "upstream"
	CodeInvalidReply = "invalid_reply"
	CodeInvalidInput = "invalid_input"
	CodeNoContent    = "no_content"
)

// Reserved failures. They are compared with errors.Is, and a code that also
// carries an upstream status still matches its sentinel.
var (
	ErrDisabled     = &Failure{Code: CodeDisabled}
	ErrUnconfigured = &Failure{Code: CodeUnconfigured}
	ErrTimeout      = &Failure{Code: CodeTimeout}
	ErrBusy         = &Failure{Code: CodeBusy}
	ErrUpstream     = &Failure{Code: CodeUpstream}
	ErrInvalidReply = &Failure{Code: CodeInvalidReply}
	ErrInvalidInput = &Failure{Code: CodeInvalidInput}
	ErrNoContent    = &Failure{Code: CodeNoContent}
)

const (
	// maxTimeout bounds one upstream call. It is the only timeout Boop offers:
	// a caller that needs less passes a shorter context deadline.
	maxTimeout = 20 * time.Second
	// maxResponseBytes caps how much of one reply is read, so a hostile or
	// broken endpoint cannot make the process allocate without bound.
	maxResponseBytes = 1 << 20
	// maxConcurrentCalls is the process-wide semaphore of in-flight model calls.
	// Two is the ceiling a 1 core / 512 MiB host can carry while the home page
	// and the writing assistant may both want a call.
	maxConcurrentCalls = 2
	// maxPromptBytes caps one prompt, so the snapshot budget plus the template
	// can never grow into an unbounded request.
	maxPromptBytes = 48 << 10
	// DefaultTTL is how long a generated author status stays fresh.
	DefaultTTL = 168 * time.Hour
	// BackgroundTimeout bounds a refresh that runs away from a request: the
	// upstream call inside it still has maxTimeout, this covers the snapshot and
	// the write that follow.
	BackgroundTimeout = 30 * time.Second
)

// Failure is a redacted AI failure: a stable code plus the upstream HTTP status
// when one was received. Nothing else is kept, so it is always safe to log or
// store.
type Failure struct {
	Code   string
	Status int
}

func (f *Failure) Error() string {
	if f.Status != 0 {
		return "ai: " + f.Code + " (http " + strconv.Itoa(f.Status) + ")"
	}
	return "ai: " + f.Code
}

// Is matches the sentinel of the same code, so a status-carrying upstream
// failure still answers errors.Is(err, ErrUpstream).
func (f *Failure) Is(target error) bool {
	other, ok := target.(*Failure)
	return ok && other.Code == f.Code
}

// Code reports the stable code of an AI failure, or "" for any other error.
func Code(err error) string {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ""
}

func failStatus(code string, status int) error {
	return &Failure{Code: code, Status: status}
}

// Config is a resolved AI configuration: the stored settings plus the decrypted
// API key. The server rebuilds it on every call, so an admin change takes effect
// without a restart and a plaintext key never outlives the call that needed it.
type Config struct {
	BaseURL string
	Model   string
	APIKey  string
	// TTL is how long a generated author status stays fresh; zero means
	// DefaultTTL.
	TTL time.Duration
}

// Configured reports whether every value an upstream call needs is present.
func (c Config) Configured() bool {
	return strings.TrimSpace(c.BaseURL) != "" && strings.TrimSpace(c.Model) != "" && c.APIKey != ""
}

func (c Config) ttl() time.Duration {
	if c.TTL <= 0 {
		return DefaultTTL
	}
	return c.TTL
}

// chatClient is the process's only outbound HTTP client. Per-call deadlines come
// from the request context, so maxTimeout is an upper bound rather than the
// effective timeout of a caller that already has a shorter one.
var chatClient = &http.Client{Timeout: maxTimeout}

// calls is the process-wide semaphore of concurrent model calls.
var calls = make(chan struct{}, maxConcurrentCalls)

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model     string    `json:"model"`
	Messages  []message `json:"messages"`
	MaxTokens int       `json:"max_tokens,omitempty"`
}

// chatResponse is the only part of an OpenAI-compatible reply Boop reads: the
// first choice's message content.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// complete makes the one upstream call Boop knows: an OpenAI-compatible chat
// completion. Every boundary lives here - input cap, timeout, concurrency,
// response cap - and every failure is reduced to a Failure code.
func complete(ctx context.Context, cfg Config, messages []message, maxTokens int) (string, error) {
	if !cfg.Configured() {
		return "", ErrUnconfigured
	}
	prompt := 0
	for _, item := range messages {
		prompt += len(item.Content)
	}
	if prompt == 0 || prompt > maxPromptBytes {
		return "", ErrInvalidInput
	}
	payload, err := json.Marshal(chatRequest{Model: cfg.Model, Messages: messages, MaxTokens: maxTokens})
	if err != nil {
		return "", ErrInvalidInput
	}

	ctx, cancel := context.WithTimeout(ctx, maxTimeout)
	defer cancel()
	if err := acquire(ctx); err != nil {
		return "", err
	}
	defer func() { <-calls }()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint(cfg.BaseURL), bytes.NewReader(payload))
	if err != nil {
		return "", ErrUnconfigured
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	// The key travels in the header only: it is never a query parameter, a log
	// field or an error message.
	request.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	response, err := chatClient.Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "", ErrTimeout
		}
		return "", ErrUpstream
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode > 299 {
		// The body may contain anything: drain a bounded amount so the
		// connection can be reused, and never keep a byte of it.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return "", failStatus(CodeUpstream, response.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "", ErrTimeout
		}
		return "", ErrUpstream
	}
	if len(body) > maxResponseBytes {
		return "", ErrInvalidReply
	}
	var parsed chatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", ErrInvalidReply
	}
	if len(parsed.Choices) == 0 {
		return "", ErrInvalidReply
	}
	content := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if content == "" {
		return "", ErrInvalidReply
	}
	return content, nil
}

// acquire takes one of the process-wide slots. A caller whose deadline passes
// while it waits is reported as busy instead of queueing behind a slow model.
func acquire(ctx context.Context) error {
	select {
	case calls <- struct{}{}:
		return nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ErrTimeout
		}
		return ErrBusy
	}
}

// endpoint joins the configured base URL with the chat completions path, with or
// without a trailing slash in the stored value.
func endpoint(base string) string {
	return strings.TrimSuffix(strings.TrimSpace(base), "/") + "/chat/completions"
}

// decodeStrict decodes exactly one JSON object with no unknown field and no
// trailing JSON, so a chatty model cannot smuggle extra structure into a
// suggestion.
func decodeStrict(content string, dst any) error {
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return ErrInvalidReply
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrInvalidReply
	}
	return nil
}

// Test makes the smallest useful call: one short completion that proves the
// endpoint, the model and the key work together. Only the configured model name
// is reported back, so no model output ever reaches a response or a log.
func Test(ctx context.Context, cfg Config) (string, error) {
	if !cfg.Configured() {
		return "", ErrUnconfigured
	}
	if _, err := complete(ctx, cfg, []message{{Role: "user", Content: "ping"}}, 8); err != nil {
		return "", err
	}
	return cfg.Model, nil
}

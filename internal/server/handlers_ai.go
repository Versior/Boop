package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"boop/internal/ai"
	"boop/internal/settings"
)

// aiJSONBytes bounds the assistant request. The draft itself is capped by
// ai.MaxInputBytes, so 64KiB only leaves room for the JSON envelope.
const aiJSONBytes = 64 << 10

// authorStatusPayload is the documented public shape of the author status
// (docs/API.md §设置与 AI). It is both the JSON response and the view data of the
// home page card.
type authorStatusPayload struct {
	Text        string   `json:"text"`
	Topics      []string `json:"topics,omitempty"`
	GeneratedAt string   `json:"generated_at"`
	Stale       bool     `json:"stale"`
	Default     bool     `json:"default"`
}

func authorStatusPayloadOf(status ai.Status) authorStatusPayload {
	return authorStatusPayload{
		Text:        status.Text,
		Topics:      status.Topics,
		GeneratedAt: status.GeneratedAt,
		Stale:       status.Stale,
		Default:     status.Default,
	}
}

// assistRequest is the strict assistant payload: the action plus the draft the
// composer already holds. A post id is deliberately absent, so an assistant call
// can never be pointed at stored content.
type assistRequest struct {
	Action  string   `json:"action"`
	Title   string   `json:"title"`
	Body    string   `json:"body"`
	Excerpt string   `json:"excerpt"`
	Tags    []string `json:"tags"`
}

// assistPayload is one suggestion. Only the field of the requested action is
// present, and applying it is the owner's explicit choice in the composer.
type assistPayload struct {
	Action         string   `json:"action"`
	Summary        string   `json:"summary,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	SEOTitle       string   `json:"seo_title,omitempty"`
	SEODescription string   `json:"seo_description,omitempty"`
}

func assistPayloadOf(suggestion ai.Suggestion) assistPayload {
	return assistPayload{
		Action:         suggestion.Action,
		Summary:        suggestion.Summary,
		Tags:           suggestion.Tags,
		SEOTitle:       suggestion.SEOTitle,
		SEODescription: suggestion.SEODescription,
	}
}

// handleAuthorStatusAPI serves the public author status. It reads the cache and
// may start one detached refresh, but it never calls the model on this request
// (docs/API.md §设置与 AI).
func (s *server) handleAuthorStatusAPI(w http.ResponseWriter, r *http.Request) {
	values := s.displaySettings(r)
	status := s.authorStatus(r.Context(), values)
	writeJSON(w, http.StatusOK, map[string]any{"data": authorStatusPayloadOf(status)})
}

// authorStatus resolves the status the home page and the public endpoint show.
// The cached value is served even when AI is disabled or unconfigured, and an
// out-of-date value is refreshed in the background so no visitor waits for the
// model.
func (s *server) authorStatus(ctx context.Context, values settings.Values) ai.Status {
	status, refresh, err := ai.Decide(ctx, s.db, time.Now())
	if err != nil {
		s.logger.LogAttrs(ctx, slog.LevelWarn, "author status unavailable",
			slog.String("code", ai.Code(err)), slog.String("error", err.Error()))
		return ai.FallbackStatus()
	}
	if !refresh {
		return status
	}
	// The key is decrypted only when a refresh actually starts, so a cached home
	// page visit never touches the secret store.
	if cfg, err := s.aiConfig(ctx, values); err == nil {
		s.aiRefresh.RefreshInBackground(s.db, cfg, s.logger)
	}
	return status
}

// aiConfig resolves the AI configuration of one call. The key is decrypted here,
// every time, so an admin change takes effect without a restart and a plaintext
// key never outlives the call. A disabled site, a missing base URL, model or key
// and a missing BOOP_MASTER_KEY all report why they are unusable.
func (s *server) aiConfig(ctx context.Context, values settings.Values) (ai.Config, error) {
	switch {
	case !values.AIEnabled:
		return ai.Config{}, ai.ErrDisabled
	case values.AIBaseURL == "" || values.AIChatModel == "":
		return ai.Config{}, ai.ErrUnconfigured
	}
	key, err := settings.ReadSecret(ctx, s.db, s.secrets, settings.SecretKeyAIAPIKey)
	if err != nil {
		s.logger.LogAttrs(ctx, slog.LevelWarn, "ai api key unavailable",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(ctx)))
		return ai.Config{}, ai.ErrUnconfigured
	}
	return ai.Config{
		BaseURL: values.AIBaseURL,
		Model:   values.AIChatModel,
		APIKey:  key,
		TTL:     time.Duration(values.AIAuthorStatusTTLHours) * time.Hour,
	}, nil
}

// aiAvailable reports whether the writing assistant may be offered at all. It
// asks the same resolution every AI route uses, so the composer only offers a
// control that can actually work: the stored key must really decrypt, and a
// missing BOOP_MASTER_KEY or a damaged ciphertext hides the control instead of
// rendering one that could only fail.
func (s *server) aiAvailable(ctx context.Context, values settings.Values) bool {
	_, err := s.aiConfig(ctx, values)
	return err == nil
}

// loadedSettings reads the settings for an owner-only AI route. Unlike a render
// path it fails closed: acting on defaults could send a request to the wrong
// endpoint or hide a real configuration.
func (s *server) loadedSettings(w http.ResponseWriter, r *http.Request, operation string) (settings.Values, bool) {
	values, err := settings.Load(r.Context(), s.db)
	if err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelError, operation+": settings unavailable",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
		return settings.Values{}, false
	}
	return values, true
}

// handleAITestAPI makes the smallest useful call, so the owner learns whether the
// endpoint, the model and the key work together. Only a safe success and the
// configured model name come back; no reply content is echoed.
func (s *server) handleAITestAPI(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.requireOwner(w, r)
	if !ok {
		return
	}
	if !s.guardRateLimit(w, r, s.limiters.ai, "ai", userKey(owner.ID), ipKey(r.RemoteAddr)) {
		return
	}
	values, ok := s.loadedSettings(w, r, "ai test")
	if !ok {
		return
	}
	cfg, err := s.aiConfig(r.Context(), values)
	if err != nil {
		s.writeAIFailure(w, r, "ai test", err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ai.BackgroundTimeout)
	defer cancel()
	model, err := ai.Test(ctx, cfg)
	if err != nil {
		s.writeAIFailure(w, r, "ai test", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"ok": true, "model": model}})
}

// handleAIRegenerateAPI performs one explicit author status refresh. The refresh
// runs on this request, because the owner asked for the result, and a refresh
// that is already in flight is answered as busy instead of starting a second
// call.
func (s *server) handleAIRegenerateAPI(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.requireOwner(w, r)
	if !ok {
		return
	}
	if !s.guardRateLimit(w, r, s.limiters.ai, "ai", userKey(owner.ID), ipKey(r.RemoteAddr)) {
		return
	}
	values, ok := s.loadedSettings(w, r, "ai regenerate")
	if !ok {
		return
	}
	cfg, err := s.aiConfig(r.Context(), values)
	if err != nil {
		s.writeAIFailure(w, r, "ai regenerate", err)
		return
	}
	if !s.aiRefresh.TryStart() {
		s.writeAIFailure(w, r, "ai regenerate", ai.ErrBusy)
		return
	}
	defer s.aiRefresh.Finish()

	ctx, cancel := context.WithTimeout(r.Context(), ai.BackgroundTimeout)
	defer cancel()
	status, err := ai.Refresh(ctx, s.db, cfg, s.logger, time.Now())
	if err != nil {
		s.writeAIFailure(w, r, "ai regenerate", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": authorStatusPayloadOf(status)})
}

// handleAIAssistAPI returns one bounded suggestion for the draft in the
// composer. It writes nothing: applying a suggestion stays an explicit action in
// the browser.
func (s *server) handleAIAssistAPI(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.requireOwner(w, r)
	if !ok {
		return
	}
	var body assistRequest
	if !s.readJSON(w, r, &body, aiJSONBytes) {
		return
	}
	if !ai.IsAction(body.Action) {
		writeFailure(w, r, http.StatusBadRequest, "invalid_action", "AI 助手只支持 summary、tags 与 seo")
		return
	}
	if !s.guardRateLimit(w, r, s.limiters.ai, "ai", userKey(owner.ID), ipKey(r.RemoteAddr)) {
		return
	}
	values, ok := s.loadedSettings(w, r, "ai assist")
	if !ok {
		return
	}
	cfg, err := s.aiConfig(r.Context(), values)
	if err != nil {
		s.writeAIFailure(w, r, "ai assist", err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ai.BackgroundTimeout)
	defer cancel()
	suggestion, err := ai.Assist(ctx, cfg, body.Action, ai.Draft{
		Title:   body.Title,
		Body:    body.Body,
		Excerpt: body.Excerpt,
		Tags:    body.Tags,
	})
	if err != nil {
		s.writeAIFailure(w, r, "ai assist", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": assistPayloadOf(suggestion)})
}

// handleAIFallback keeps every /api/v1/ai answer JSON, so a wrong method is a
// JSON 405 with Allow instead of ServeMux plain text.
func (s *server) handleAIFallback(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v1/ai/author-status" {
		w.Header().Set("Allow", http.MethodGet)
		writeFailure(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "该接口不支持此请求方法")
		return
	}
	writeFailure(w, r, http.StatusNotFound, "not_found", "请求的资源不存在")
}

// writeAIFailure maps an AI failure onto the documented envelope. The message is
// chosen from the stable code alone, so an upstream body is never repeated to a
// client; only codes and statuses are logged.
func (s *server) writeAIFailure(w http.ResponseWriter, r *http.Request, operation string, err error) {
	var failure *ai.Failure
	if errors.As(err, &failure) {
		status, code, message := aiErrorResponse(failure)
		if status >= http.StatusInternalServerError {
			s.logger.LogAttrs(r.Context(), slog.LevelWarn, operation+" failed",
				slog.String("code", failure.Code), slog.Int("upstream_status", failure.Status),
				slog.String("request_id", requestIDFrom(r.Context())))
		}
		writeFailure(w, r, status, code, message)
		return
	}
	s.logger.LogAttrs(r.Context(), slog.LevelError, operation+" failed",
		slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
	writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
}

// aiErrorResponse is the one translation table from an AI failure code to the
// HTTP status, machine code and Chinese message of the API.
func aiErrorResponse(failure *ai.Failure) (int, string, string) {
	switch failure.Code {
	case ai.CodeDisabled:
		return http.StatusConflict, "ai_disabled", "站点未启用 AI 功能"
	case ai.CodeUnconfigured:
		return http.StatusConflict, "ai_unconfigured", "AI 服务尚未配置完成（Base URL、模型与 API Key）"
	case ai.CodeBusy:
		return http.StatusConflict, "ai_busy", "已有一个 AI 任务在进行中，请稍后再试"
	case ai.CodeNoContent:
		return http.StatusConflict, "ai_no_content", "还没有已发布的内容可以用于生成作者状态"
	case ai.CodeInvalidInput:
		return http.StatusBadRequest, "invalid_body", "请先填写需要处理的内容"
	case ai.CodeTimeout:
		return http.StatusGatewayTimeout, "ai_timeout", "AI 服务响应超时，请稍后再试"
	case ai.CodeInvalidReply:
		return http.StatusBadGateway, "ai_invalid_reply", "AI 服务返回的内容无法解析"
	default:
		return http.StatusBadGateway, "ai_upstream_error", "AI 服务暂时不可用"
	}
}

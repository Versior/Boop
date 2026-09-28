package ai

import (
	"context"
	"strings"
)

// Writing assistant actions (docs/API.md §设置与 AI).
const (
	ActionSummary = "summary"
	ActionTags    = "tags"
	ActionSEO     = "seo"
)

// Assistant bounds. They are the documented limits of one suggestion and of one
// request: a draft larger than MaxInputBytes is rejected before any upstream
// call, and a suggestion is bounded before it is rendered.
const (
	maxSummaryRunes        = 300
	maxTags                = 8
	maxTagRunes            = 30
	maxSEOTitleRunes       = 160
	maxSEODescriptionRunes = 300
	// MaxInputBytes caps the draft one request may carry. The composer's own
	// body limit is larger, so the assistant budget is stated separately instead
	// of being inherited.
	MaxInputBytes = 16 << 10
	// maxAssistTokens leaves room for one bounded suggestion.
	maxAssistTokens = 700
)

// assistSystemPrompt keeps every reply machine-readable: strict JSON is decoded
// with unknown fields and trailing content rejected.
const assistSystemPrompt = "你是个人博客 Boop 的写作助手。只输出一个 JSON 对象，不要输出解释、Markdown 代码块或多余文字。"

// IsAction reports whether the action is one the assistant implements.
func IsAction(action string) bool {
	switch action {
	case ActionSummary, ActionTags, ActionSEO:
		return true
	default:
		return false
	}
}

// Draft is the bounded draft content the owner asks about. It is a copy of what
// is in the composer: the assistant never reads or writes a post itself.
type Draft struct {
	Title   string
	Body    string
	Excerpt string
	Tags    []string
}

// Suggestion is one assistant answer. Only the field of the requested action is
// filled, so a response never mixes a summary into a tag list.
type Suggestion struct {
	Action         string
	Summary        string
	Tags           []string
	SEOTitle       string
	SEODescription string
}

// Assist asks the model for one suggestion about a draft. The result is bounded
// and is never written anywhere: applying it is the owner's explicit choice.
func Assist(ctx context.Context, cfg Config, action string, draft Draft) (Suggestion, error) {
	prompt, err := draftPrompt(action, draft)
	if err != nil {
		return Suggestion{}, err
	}
	reply, err := complete(ctx, cfg, []message{
		{Role: "system", Content: assistSystemPrompt},
		{Role: "user", Content: prompt},
	}, maxAssistTokens)
	if err != nil {
		return Suggestion{}, err
	}
	return parseSuggestion(action, reply)
}

// draftPrompt renders the bounded prompt of one action. An empty or oversized
// draft is rejected here, so a request that cannot produce a useful answer never
// reaches the model.
func draftPrompt(action string, draft Draft) (string, error) {
	if !IsAction(action) {
		return "", ErrInvalidInput
	}
	if err := validateDraft(draft); err != nil {
		return "", err
	}

	var b strings.Builder
	if title := strings.TrimSpace(draft.Title); title != "" {
		b.WriteString("标题：" + title + "\n")
	}
	if excerpt := strings.TrimSpace(draft.Excerpt); excerpt != "" {
		b.WriteString("现有摘要：" + excerpt + "\n")
	}
	if tags := boundList(draft.Tags, maxTags, maxTagRunes); len(tags) > 0 {
		b.WriteString("现有标签：" + strings.Join(tags, "、") + "\n")
	}
	if body := strings.TrimSpace(draft.Body); body != "" {
		b.WriteString("正文：\n" + body + "\n")
	}

	switch action {
	case ActionSummary:
		b.WriteString("\n请写一段不超过 300 字的中文摘要，只输出 {\"summary\":\"...\"}。")
	case ActionTags:
		b.WriteString("\n请给出不超过 8 个中文标签，每个不超过 30 字，只输出 {\"tags\":[\"...\"]}。")
	case ActionSEO:
		b.WriteString("\n请给出 SEO 标题（不超过 160 字）与描述（不超过 300 字），只输出 {\"title\":\"...\",\"description\":\"...\"}。")
	}
	return b.String(), nil
}

// validateDraft enforces the input bounds: something to work with, and no more
// bytes than the documented budget.
func validateDraft(draft Draft) error {
	total := len(draft.Title) + len(draft.Body) + len(draft.Excerpt)
	for _, tag := range draft.Tags {
		total += len(tag)
	}
	if total > MaxInputBytes {
		return ErrInvalidInput
	}
	empty := strings.TrimSpace(draft.Title) == "" && strings.TrimSpace(draft.Body) == "" &&
		strings.TrimSpace(draft.Excerpt) == "" && len(boundList(draft.Tags, maxTags, maxTagRunes)) == 0
	if empty {
		return ErrInvalidInput
	}
	return nil
}

// parseSuggestion decodes the strict reply of one action and bounds it.
func parseSuggestion(action, reply string) (Suggestion, error) {
	switch action {
	case ActionSummary:
		var payload struct {
			Summary string `json:"summary"`
		}
		if err := decodeStrict(reply, &payload); err != nil {
			return Suggestion{}, err
		}
		summary := truncateRunes(strings.TrimSpace(payload.Summary), maxSummaryRunes)
		if summary == "" {
			return Suggestion{}, ErrInvalidReply
		}
		return Suggestion{Action: action, Summary: summary}, nil
	case ActionTags:
		var payload struct {
			Tags []string `json:"tags"`
		}
		if err := decodeStrict(reply, &payload); err != nil {
			return Suggestion{}, err
		}
		tags := boundList(payload.Tags, maxTags, maxTagRunes)
		if len(tags) == 0 {
			return Suggestion{}, ErrInvalidReply
		}
		return Suggestion{Action: action, Tags: tags}, nil
	case ActionSEO:
		var payload struct {
			Title       string `json:"title"`
			Description string `json:"description"`
		}
		if err := decodeStrict(reply, &payload); err != nil {
			return Suggestion{}, err
		}
		title := truncateRunes(strings.TrimSpace(payload.Title), maxSEOTitleRunes)
		description := truncateRunes(strings.TrimSpace(payload.Description), maxSEODescriptionRunes)
		if title == "" && description == "" {
			return Suggestion{}, ErrInvalidReply
		}
		return Suggestion{Action: action, SEOTitle: title, SEODescription: description}, nil
	default:
		return Suggestion{}, ErrInvalidInput
	}
}

// Package content owns Boop posts: type-specific validation, slug allocation,
// Markdown rendering, soft deletion and the public feed/detail queries.
package content

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Post types and statuses (docs/DATABASE.md).
const (
	TypeMoment  = "moment"
	TypeArticle = "article"
	TypePhoto   = "photo"

	StatusDraft     = "draft"
	StatusPublished = "published"
	StatusArchived  = "archived"
)

// Validation bounds. docs/PRODUCT.md fixes the moment length; the remaining
// limits are Boop's own deterministic bounds so every rejected write has a
// precise reason instead of relying on the transport size limit.
const (
	MaxMomentRunes   = 2000
	MaxBodyBytes     = 256 << 10
	MaxTitleRunes    = 160
	MaxExcerptRunes  = 300
	MaxTagRunes      = 30
	MaxTags          = 10
	MaxLocationRunes = 120
	MaxAssets        = 20
	MaxSlugRunes     = 80
	maxSlugAttempts  = 20
	DefaultPageSize  = 20
	MaxPageSize      = 50
)

// TimestampFormat is the stored timestamp layout: RFC3339 with sub-second
// precision, so two writes in the same second still have distinct updated_at
// values and the optimistic lock stays meaningful.
const TimestampFormat = time.RFC3339Nano

// ErrNotFound is returned for unknown, deleted or unpublished posts.
var ErrNotFound = errors.New("content: post not found")

// ErrConflict reports a failed optimistic lock check.
var ErrConflict = errors.New("content: post was modified by another request")

// ValidationError carries the machine code and the user-facing message of a
// rejected write so the HTTP layer stays a thin translation.
type ValidationError struct {
	Code    string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("content: %s: %s", e.Code, e.Message)
}

func invalid(code, message string) *ValidationError {
	return &ValidationError{Code: code, Message: message}
}

// Asset is the stored-file metadata a post references. Uploads are Task 5, so
// only existing rows can be linked.
type Asset struct {
	ID         int64
	StorageKey string
	MimeType   string
	AltText    string
	SizeBytes  int64
	Width      *int
	Height     *int
}

// Post is one piece of content with its feed relations resolved.
type Post struct {
	ID             int64
	Slug           string
	Type           string
	Status         string
	Title          string
	BodyMarkdown   string
	BodyHTML       string
	Excerpt        string
	CoverAssetID   *int64
	Location       string
	CapturedAt     string
	SEOTitle       string
	SEODescription string
	PublishedAt    string
	CreatedAt      string
	UpdatedAt      string
	LikeCount      int
	CommentCount   int
	Assets         []Asset
	Tags           []string
}

// Published reports whether the post is publicly visible.
func (p Post) Published() bool {
	return p.Status == StatusPublished
}

// Cover returns the asset used as the feed image, if any.
func (p Post) Cover() (Asset, bool) {
	for _, asset := range p.Assets {
		if p.CoverAssetID != nil && asset.ID == *p.CoverAssetID {
			return asset, true
		}
	}
	if len(p.Assets) > 0 {
		return p.Assets[0], true
	}
	return Asset{}, false
}

// Input is a full create payload.
type Input struct {
	Type       string
	Status     string
	Title      string
	Body       string
	Excerpt    string
	AssetIDs   []int64
	Tags       []string
	Location   string
	CapturedAt string
}

// Patch is a partial update. UpdatedAt is the optimistic lock the caller read;
// Type is deliberately absent because a post never changes type.
type Patch struct {
	UpdatedAt  string
	Status     *string
	Title      *string
	Body       *string
	Excerpt    *string
	AssetIDs   *[]int64
	Tags       *[]string
	Location   *string
	CapturedAt *string
}

// validated holds a create payload after normalization and range checks.
type validated struct {
	Input
	publishing bool
}

// validateInput normalizes a create payload and enforces the documented rules:
// status/type membership, per-type completeness when publishing, and bounded
// text fields.
func validateInput(in Input) (validated, error) {
	in.Type = strings.TrimSpace(in.Type)
	in.Status = strings.TrimSpace(in.Status)
	in.Title = strings.TrimSpace(in.Title)
	in.Body = strings.TrimSpace(in.Body)
	in.Excerpt = strings.TrimSpace(in.Excerpt)
	in.Location = strings.TrimSpace(in.Location)
	in.CapturedAt = strings.TrimSpace(in.CapturedAt)

	switch in.Type {
	case TypeMoment, TypeArticle, TypePhoto:
	default:
		return validated{}, invalid("invalid_type", "内容类型不存在")
	}
	switch in.Status {
	case StatusDraft, StatusPublished, StatusArchived:
	default:
		return validated{}, invalid("invalid_status", "内容状态不存在")
	}

	// Moments and photos have no headline in the product model; storing one
	// would silently discard author input.
	if in.Title != "" && in.Type != TypeArticle {
		return validated{}, invalid("invalid_title", "只有文章可以设置标题")
	}
	if runes := utf8.RuneCountInString(in.Title); runes > MaxTitleRunes {
		return validated{}, invalid("invalid_title", fmt.Sprintf("标题最多 %d 个字符", MaxTitleRunes))
	}
	if len(in.Body) > MaxBodyBytes {
		return validated{}, invalid("invalid_body", "正文过长")
	}
	if in.Type == TypeMoment {
		if runes := utf8.RuneCountInString(in.Body); runes > MaxMomentRunes {
			return validated{}, invalid("invalid_body", fmt.Sprintf("动态最多 %d 个字", MaxMomentRunes))
		}
	}
	if runes := utf8.RuneCountInString(in.Excerpt); runes > MaxExcerptRunes {
		return validated{}, invalid("invalid_excerpt", fmt.Sprintf("摘要最多 %d 个字符", MaxExcerptRunes))
	}
	if runes := utf8.RuneCountInString(in.Location); runes > MaxLocationRunes {
		return validated{}, invalid("invalid_location", fmt.Sprintf("地点最多 %d 个字符", MaxLocationRunes))
	}
	if in.CapturedAt != "" {
		if _, err := time.Parse(TimestampFormat, in.CapturedAt); err != nil {
			return validated{}, invalid("invalid_captured_at", "拍摄时间必须是 RFC3339 时间")
		}
	}

	ids, err := normalizeAssetIDs(in.AssetIDs)
	if err != nil {
		return validated{}, err
	}
	in.AssetIDs = ids

	tags, err := normalizeTags(in.Tags)
	if err != nil {
		return validated{}, err
	}
	in.Tags = tags

	publishing := in.Status == StatusPublished
	if publishing {
		switch in.Type {
		case TypeArticle:
			if in.Title == "" {
				return validated{}, invalid("invalid_title", "发布文章需要标题")
			}
			if in.Body == "" {
				return validated{}, invalid("invalid_body", "发布文章需要正文")
			}
		case TypeMoment:
			if in.Body == "" {
				return validated{}, invalid("invalid_body", "发布动态需要正文")
			}
		case TypePhoto:
			if len(in.AssetIDs) == 0 {
				return validated{}, invalid("invalid_assets", "发布摄影内容至少需要一张图片")
			}
		}
	}

	return validated{Input: in, publishing: publishing}, nil
}

func normalizeAssetIDs(ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > MaxAssets {
		return nil, invalid("invalid_assets", fmt.Sprintf("最多关联 %d 个资源", MaxAssets))
	}
	seen := make(map[int64]bool, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, invalid("invalid_asset", "资源 ID 不正确")
		}
		if seen[id] {
			return nil, invalid("invalid_asset", "同一个资源不能重复关联")
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}

func normalizeTags(tags []string) ([]string, error) {
	if len(tags) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		trimmed := strings.TrimSpace(tag)
		if trimmed == "" {
			return nil, invalid("invalid_tags", "标签不能为空")
		}
		if runes := utf8.RuneCountInString(trimmed); runes > MaxTagRunes {
			return nil, invalid("invalid_tags", fmt.Sprintf("标签最多 %d 个字符", MaxTagRunes))
		}
		key := strings.ToLower(trimmed)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, trimmed)
	}
	if len(out) > MaxTags {
		return nil, invalid("invalid_tags", fmt.Sprintf("最多 %d 个标签", MaxTags))
	}
	return out, nil
}

// Slugify turns a title or body fragment into a URL path segment: lower case,
// letters/digits/CJK kept, everything else collapsed into single hyphens.
func Slugify(source string) string {
	var b strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(source)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen && b.Len() > 0 {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
		if utf8.RuneCountInString(b.String()) >= MaxSlugRunes {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// slugBase picks the source text for a new slug. Moments usually have no
// headline, so their body supplies a readable fragment.
func slugBase(in Input) string {
	if in.Title != "" {
		return in.Title
	}
	if in.Body != "" {
		return in.Body
	}
	return in.Location
}

// fallbackSlug is used when the source text has no usable characters.
func fallbackSlug(postType string, at time.Time) string {
	return postType + "-" + strings.ToLower(at.UTC().Format("20060102t150405"))
}

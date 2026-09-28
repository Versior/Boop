package server

import (
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"boop/internal/media"
)

const (
	// uploadFieldName is the single multipart field the client sends.
	uploadFieldName = "file"

	// uploadsPath is the public prefix of every stored file. Task 4 already
	// publishes asset URLs as uploadsPrefix + storage key.
	uploadsPath = uploadsPrefix
)

// uploadPayload is the response of a successful upload.
type uploadPayload struct {
	ID           int64  `json:"id"`
	URL          string `json:"url"`
	StorageKey   string `json:"storage_key"`
	MimeType     string `json:"mime_type"`
	SizeBytes    int64  `json:"size_bytes"`
	Width        *int   `json:"width"`
	Height       *int   `json:"height"`
	OriginalName string `json:"original_name"`
	Reused       bool   `json:"reused"`
}

// handleUploadAPI stores one image for the owner. Authentication, role, CSRF
// and the request-size ceiling are handled by the shared middleware; this
// handler adds the single-file contract and the content checks.
func (s *server) handleUploadAPI(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.requireOwner(w, r)
	if !ok {
		return
	}
	limit := s.cfg.MaxUploadBytes()
	filename, data, ok := s.readUpload(w, r, limit)
	if !ok {
		return
	}

	asset, err := media.Store(r.Context(), s.db,
		media.Options{DataDir: s.cfg.DataDir, MaxBytes: limit},
		owner.ID, filename, data, time.Now())
	if err != nil {
		s.writeUploadFailure(w, r, err)
		return
	}

	status := http.StatusCreated
	if asset.Reused {
		// The bytes were already stored for this owner: nothing new was created.
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"data": uploadPayload{
		ID: asset.ID, URL: uploadsPath + asset.StorageKey, StorageKey: asset.StorageKey,
		MimeType: asset.MimeType, SizeBytes: asset.SizeBytes, Width: asset.Width, Height: asset.Height,
		OriginalName: asset.OriginalName, Reused: asset.Reused,
	}})
}

// readUpload streams the single file part into memory, enforcing the upload
// budget while reading so an oversized body is never buffered whole.
func (s *server) readUpload(w http.ResponseWriter, r *http.Request, limit int64) (string, []byte, bool) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") || params["boundary"] == "" {
		writeFailure(w, r, http.StatusBadRequest, "invalid_body", "请以 multipart/form-data 上传单个文件")
		return "", nil, false
	}
	reader, err := r.MultipartReader()
	if err != nil {
		writeFailure(w, r, http.StatusBadRequest, "invalid_body", "请以 multipart/form-data 上传单个文件")
		return "", nil, false
	}

	var filename string
	var data []byte
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.writeUploadReadFailure(w, r, err)
			return "", nil, false
		}
		if part.FileName() == "" {
			// A plain form field: the contract is exactly one file named
			// uploadFieldName, extra fields are ignored.
			continue
		}
		if part.FormName() != uploadFieldName {
			writeFailure(w, r, http.StatusBadRequest, "invalid_body", "文件字段名必须是 "+uploadFieldName)
			return "", nil, false
		}
		if filename != "" {
			writeFailure(w, r, http.StatusBadRequest, "invalid_body", "一次只能上传一个文件")
			return "", nil, false
		}
		filename = part.FileName()
		body, err := io.ReadAll(io.LimitReader(part, limit+1))
		if err != nil {
			s.writeUploadReadFailure(w, r, err)
			return "", nil, false
		}
		if int64(len(body)) > limit {
			writeFailure(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "文件超过上传上限")
			return "", nil, false
		}
		data = body
	}

	if filename == "" || len(data) == 0 {
		writeFailure(w, r, http.StatusBadRequest, "invalid_body", "请选择一个文件")
		return "", nil, false
	}
	return filename, data, true
}

// writeUploadReadFailure maps a failed multipart read: over budget is 413,
// anything else is a malformed body.
func (s *server) writeUploadReadFailure(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeFailure(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "请求体超过大小上限")
		return
	}
	writeFailure(w, r, http.StatusBadRequest, "invalid_body", "上传数据不完整")
}

// writeUploadFailure maps media errors onto the documented envelope.
func (s *server) writeUploadFailure(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, media.ErrUnsupportedMedia):
		writeFailure(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type", "只支持 jpg、png、webp、gif 图片")
	case errors.Is(err, media.ErrInvalidFilename):
		writeFailure(w, r, http.StatusBadRequest, "invalid_filename", "文件扩展名与实际内容不一致")
	case errors.Is(err, media.ErrTooLarge):
		writeFailure(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "文件超过上传上限")
	default:
		s.logger.LogAttrs(r.Context(), slog.LevelError, "upload failed",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
	}
}

// handleUploads serves a stored file to anyone: article images are public and
// need no session. The key shape is the traversal defense, and every key is
// written once and never rewritten, so the response is immutable.
func (s *server) handleUploads(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	contentType := media.ContentType(key)
	file, info, err := media.Open(media.Options{DataDir: s.cfg.DataDir}, key)
	if err != nil {
		writeFailure(w, r, http.StatusNotFound, "not_found", "请求的资源不存在")
		return
	}
	defer file.Close()

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, path.Base(key), info.ModTime(), file)
}

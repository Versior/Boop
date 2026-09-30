package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"boop/web"
)

// staticDirPrefix is the embedded directory the static assets live under.
const staticDirPrefix = "static/"

// staticAsset is one embedded file: the bytes as stored, the gzip-encoded copy
// when compressing helped, and one strong entity tag per representation.
//
// The tags differ on purpose. A single tag for both would let a shared cache
// hand a gzip body to a client that never asked for one, because the tag is what
// the cache keys a revalidation on; appending a suffix per coding keeps each tag
// describing exactly the bytes it labels.
type staticAsset struct {
	contentType string
	body        []byte
	gzipped     []byte
	etag        string
	gzipETag    string
}

// staticBundle is the whole of web/static in the form the handler serves it.
type staticBundle struct {
	files map[string]staticAsset
	// version is a short content-derived token. The templates append it to every
	// asset URL, which is what makes a one-year immutable cache safe on a name
	// that never changes: a deploy changes the version, so the URL changes with
	// it and no client is stuck on the previous bytes.
	version string
}

// staticAssets compiles the bundle once per process. It is a pure function of
// the embedded bytes - no configuration, no clock, no I/O - so memoising it
// costs nothing in correctness and keeps every test server from re-compressing
// the same 64 KB.
var staticAssets = sync.OnceValue(compileStaticAssets)

func compileStaticAssets() staticBundle {
	bundle := staticBundle{files: make(map[string]staticAsset)}
	sum := sha256.New()

	err := fs.WalkDir(web.FS, "static", func(p string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := fs.ReadFile(web.FS, p)
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(p, staticDirPrefix)
		digest := sha256.Sum256(body)
		asset := staticAsset{
			contentType: staticContentType(name),
			body:        body,
			etag:        `"` + hex.EncodeToString(digest[:]) + `"`,
		}
		if compressed, ok := gzipCopy(body); ok {
			asset.gzipped = compressed
			asset.gzipETag = `"` + hex.EncodeToString(digest[:]) + `-gzip"`
		}
		bundle.files[name] = asset

		// The version covers the name as well as the bytes, so adding or removing
		// a file also moves it.
		sum.Write([]byte(name))
		sum.Write([]byte{0})
		sum.Write(digest[:])
		sum.Write([]byte{0})
		return nil
	})
	if err != nil {
		// The assets are embedded, so a failure here is a build-time invariant
		// rather than a runtime state: the same panic path the templates use.
		panic("server: compile static assets: " + err.Error())
	}
	bundle.version = hex.EncodeToString(sum.Sum(nil))[:12]
	return bundle
}

// gzipCopy returns the gzip-encoded form of body, or ok=false when compressing
// did not make it smaller. Only text assets are worth offering, but trying is
// cheaper than a suffix allowlist and the size test settles it either way.
func gzipCopy(body []byte) ([]byte, bool) {
	var buffer bytes.Buffer
	writer, err := gzip.NewWriterLevel(&buffer, gzip.BestCompression)
	if err != nil {
		return nil, false
	}
	if _, err := writer.Write(body); err != nil {
		return nil, false
	}
	if err := writer.Close(); err != nil {
		return nil, false
	}
	if buffer.Len() >= len(body) {
		return nil, false
	}
	return buffer.Bytes(), true
}

// staticURL turns a stored brand image address into the form a page should link
// to. A path into the embedded bundle is served with a one-year immutable
// cache, which is only safe because the URL carries a content-derived version -
// so the version has to be attached here, at the moment the address becomes a
// link. The shipped cover image is exactly such a path, and without this a
// deployment that replaced that banner would leave every returning visitor
// looking at the previous bytes for a year.
//
// Anything else passes through untouched: an absolute URL belongs to someone
// else's cache policy, and a path the site serves itself (/uploads/…) is
// addressed by its own name, so a version query would mean nothing to the
// handler that answers it.
func staticURL(image string) string {
	if !strings.HasPrefix(image, "/"+staticDirPrefix) {
		return image
	}
	return image + "?v=" + staticAssets().version
}

// handleStatic serves the embedded assets. Every asset is written once at build
// time and the URL carries a content-derived version, so the response is both
// immutable and impossible to pin to stale bytes.
func (s *server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	if name == r.URL.Path || !fs.ValidPath(name) {
		writeFailure(w, r, http.StatusNotFound, "not_found", "请求的资源不存在")
		return
	}
	asset, ok := staticAssets().files[name]
	if !ok {
		writeFailure(w, r, http.StatusNotFound, "not_found", "请求的资源不存在")
		return
	}

	header := w.Header()
	header.Set("Content-Type", asset.contentType)
	header.Set("Cache-Control", immutableCacheControl)
	// Two representations exist for the same URL, so both the ETag and Vary have
	// to distinguish them.
	header.Set("Vary", "Accept-Encoding")

	body := asset.body
	// A ranged request is always answered with the identity representation. A
	// byte range addresses the selected representation, so slicing the gzip
	// stream would hand the client a body it cannot inflate, and compressing
	// after slicing is not possible here because the range is only known to
	// ServeContent. Accept-Encoding is a preference rather than a requirement,
	// so declining the coding for this one request is the safe answer.
	if len(asset.gzipped) > 0 && r.Header.Get("Range") == "" && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		body = asset.gzipped
		header.Set("Content-Encoding", "gzip")
		header.Set("Etag", asset.gzipETag)
	} else {
		header.Set("Etag", asset.etag)
	}

	// ServeContent states the length itself only for an unencoded response, and
	// it overwrites this value when it slices a range, so setting it here only
	// affects the compressed case - which would otherwise go out chunked.
	header.Set("Content-Length", strconv.Itoa(len(body)))

	// ServeContent answers If-None-Match and Range against the Etag just set.
	// The modification time is the zero value because embed.FS has none, so no
	// Last-Modified is emitted and the ETag is the only validator - which is
	// also why the previous version of this handler could not just hand the
	// embedded file to http.ServeFile.
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
}

// acceptsGzip reports whether the client asked for a gzip response. An explicit
// "gzip;q=0" or "*;q=0" is a refusal, so the parameter cannot be ignored: a
// proxy that sends one would otherwise receive bytes it declared it cannot read.
func acceptsGzip(header string) bool {
	wildcard := false
	for _, part := range strings.Split(header, ",") {
		token, parameters, _ := strings.Cut(part, ";")
		token = strings.TrimSpace(strings.ToLower(token))
		quality, ok := qualityOf(parameters)
		if !ok {
			continue
		}
		switch token {
		case "gzip":
			return quality > 0
		case "*":
			wildcard = quality > 0
		}
	}
	return wildcard
}

// qualityOf reads the q parameter of one Accept-Encoding entry. A missing or
// unparseable q counts as 1, which is what RFC 9110 says.
func qualityOf(parameters string) (float64, bool) {
	for _, parameter := range strings.Split(parameters, ";") {
		name, value, found := strings.Cut(parameter, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(name), "q") {
			continue
		}
		quality, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return 1, true
		}
		return quality, true
	}
	return 1, true
}

func staticContentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/x-icon"
	default:
		return "application/octet-stream"
	}
}

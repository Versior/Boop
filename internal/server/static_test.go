package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"boop/web"
)

// staticRequest drives the static handler directly so a test can set the
// negotiation headers the shell-level helper does not expose.
func staticRequest(t *testing.T, srv *server, target string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for name, value := range header {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	srv.handleStatic(rec, req)
	return rec
}

func TestStaticAssetsAreServedImmutable(t *testing.T) {
	handler := testServer(t, discardLogger()).handler()

	for _, name := range []string{"app.css", "app.js", "brand/boop-mark.svg"} {
		t.Run(name, func(t *testing.T) {
			rec := do(t, handler, http.MethodGet, "/static/"+name, nil)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			// The URL carries a content-derived version and the bytes behind a
			// given version never change, so the cache directive can be the
			// strongest one HTTP offers.
			if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
				t.Errorf("Cache-Control = %q", got)
			}
			// Two representations answer this URL, so a shared cache has to key
			// on the coding header as well as the path.
			if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
				t.Errorf("Vary = %q, want Accept-Encoding", got)
			}
			if rec.Header().Get("Etag") == "" {
				t.Error("no Etag: an immutable asset still needs a validator for ranges")
			}
			// embed.FS carries no modification times, so there is nothing honest
			// to put in Last-Modified.
			if got := rec.Header().Get("Last-Modified"); got != "" {
				t.Errorf("Last-Modified = %q, want empty for an embedded asset", got)
			}
		})
	}
}

func TestStaticETagIsTheDigestOfTheBytesServed(t *testing.T) {
	srv := testServer(t, discardLogger())

	for _, name := range []string{"app.css", "app.js", "brand/boop-mark.svg"} {
		t.Run(name, func(t *testing.T) {
			rec := staticRequest(t, srv, "/static/"+name, nil)

			body, err := io.ReadAll(rec.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			digest := sha256.Sum256(body)
			want := `"` + hex.EncodeToString(digest[:]) + `"`
			if got := rec.Header().Get("Etag"); got != want {
				t.Errorf("Etag = %q, want the sha256 of the body (%q)", got, want)
			}
		})
	}
}

func TestStaticGzipIsASeparateRepresentation(t *testing.T) {
	srv := testServer(t, discardLogger())

	plain := staticRequest(t, srv, "/static/app.css", nil)
	compressed := staticRequest(t, srv, "/static/app.css", map[string]string{"Accept-Encoding": "gzip, deflate, br"})

	if got := compressed.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if compressed.Body.Len() >= plain.Body.Len() {
		t.Errorf("gzip body is %d bytes, plain body is %d: compressing did not help",
			compressed.Body.Len(), plain.Body.Len())
	}

	// The point of the suffix: a cache that keys a revalidation on the tag must
	// not be able to hand the gzip bytes to a client that only asked for the
	// plain ones.
	plainTag := plain.Header().Get("Etag")
	gzipTag := compressed.Header().Get("Etag")
	if plainTag == gzipTag {
		t.Errorf("both representations share Etag %q", plainTag)
	}
	if !strings.HasSuffix(gzipTag, `-gzip"`) {
		t.Errorf("gzip Etag = %q, want a -gzip suffix", gzipTag)
	}

	// The bytes have to decompress back to exactly what the plain response
	// serves, otherwise the suffix would be labelling the wrong thing.
	reader, err := gzip.NewReader(bytes.NewReader(compressed.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if string(decoded) != plain.Body.String() {
		t.Error("the gunzipped body differs from the plain body")
	}
}

func TestStaticRejectsGzipWhenTheClientRefusesIt(t *testing.T) {
	srv := testServer(t, discardLogger())

	for _, tt := range []struct {
		label  string
		header string
	}{
		{"explicit zero", "gzip;q=0"},
		{"zero with identity", "gzip; q=0.0, identity"},
		{"wildcard zero", "*;q=0"},
		{"identity only", "identity"},
		{"absent", ""},
	} {
		t.Run(tt.label, func(t *testing.T) {
			rec := staticRequest(t, srv, "/static/app.css", map[string]string{"Accept-Encoding": tt.header})

			if got := rec.Header().Get("Content-Encoding"); got != "" {
				t.Errorf("Accept-Encoding %q still got Content-Encoding %q", tt.header, got)
			}
			if !strings.Contains(rec.Body.String(), "--accent") {
				t.Error("body is not the plain asset")
			}
		})
	}
}

func TestStaticConditionalRequestsUseTheNegotiatedTag(t *testing.T) {
	srv := testServer(t, discardLogger())
	gzipTag := staticRequest(t, srv, "/static/app.css", map[string]string{"Accept-Encoding": "gzip"}).Header().Get("Etag")

	// A client that echoes the tag it was given, on the same coding, is current.
	revalidated := staticRequest(t, srv, "/static/app.css", map[string]string{
		"Accept-Encoding": "gzip",
		"If-None-Match":   gzipTag,
	})
	if revalidated.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", revalidated.Code)
	}
	if got := revalidated.Header().Get("Etag"); got != gzipTag {
		t.Errorf("304 Etag = %q, want %q", got, gzipTag)
	}
	if revalidated.Body.Len() != 0 {
		t.Errorf("304 carried %d bytes of body", revalidated.Body.Len())
	}

	// The same tag on the plain coding names different bytes, so it must not be
	// treated as a match.
	mismatched := staticRequest(t, srv, "/static/app.css", map[string]string{"If-None-Match": gzipTag})
	if mismatched.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: the tag names the gzip bytes, not these", mismatched.Code)
	}

	// And a tag for bytes that no longer exist gets the current bytes back.
	stale := staticRequest(t, srv, "/static/app.css", map[string]string{
		"Accept-Encoding": "gzip",
		"If-None-Match":   `"deadbeef"`,
	})
	if stale.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for a non-matching tag", stale.Code)
	}
}

func TestAcceptsGzipNegotiation(t *testing.T) {
	for _, tt := range []struct {
		label  string
		header string
		want   bool
	}{
		{"bare", "gzip", true},
		{"uppercase", "GZIP", true},
		{"with others", "gzip, deflate, br", true},
		{"after another", "deflate, gzip", true},
		{"explicit one", "gzip;q=1.0", true},
		{"fractional", "gzip;q=0.5, identity", true},
		{"wildcard", "*", true},
		{"wildcard fraction", "*;q=0.8", true},
		{"refused", "gzip;q=0", false},
		{"refused with space", "gzip; q=0.0, identity", false},
		{"wildcard refused", "*;q=0", false},
		{"identity only", "identity", false},
		{"others only", "deflate, br", false},
		{"absent", "", false},
		// A malformed q is treated as 1 per RFC 9110, so this still opts in.
		{"malformed q", "gzip;q=abc", true},
	} {
		t.Run(tt.label, func(t *testing.T) {
			if got := acceptsGzip(tt.header); got != tt.want {
				t.Errorf("acceptsGzip(%q) = %v, want %v", tt.header, got, tt.want)
			}
		})
	}
}

func TestStaticGzipStatesItsOwnLength(t *testing.T) {
	srv := testServer(t, discardLogger())

	rec := staticRequest(t, srv, "/static/app.css", map[string]string{"Accept-Encoding": "gzip"})
	// Go's ServeContent refuses to state a length for an encoded response, so
	// without the explicit header this goes out chunked. Compressed bytes are
	// counted in compressed bytes, which is what the header has to say.
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(rec.Body.Len()) {
		t.Errorf("Content-Length = %q, body is %d bytes", got, rec.Body.Len())
	}
}

func TestStaticRangesFallBackToTheIdentityRepresentation(t *testing.T) {
	srv := testServer(t, discardLogger())
	full := staticRequest(t, srv, "/static/app.css", nil).Body.Bytes()

	// A byte range addresses the selected representation, so answering one with
	// a slice of the gzip stream would be a body no client can inflate. The
	// coding is dropped instead, and the range is served over the raw bytes.
	rec := staticRequest(t, srv, "/static/app.css", map[string]string{
		"Accept-Encoding": "gzip",
		"Range":           "bytes=0-9",
	})

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want the identity representation", got)
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, full[:10]) {
		t.Errorf("range body = %q, want the first ten raw bytes %q", got, full[:10])
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 0-9/"+strconv.Itoa(len(full)) {
		t.Errorf("Content-Range = %q, want bytes 0-9/%d", got, len(full))
	}
}

func TestStaticVersionReachesEveryAssetURL(t *testing.T) {
	version := staticAssets().version
	if len(version) != 12 {
		t.Fatalf("version = %q, want 12 characters", version)
	}
	for _, r := range version {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("version = %q, want lowercase hex", version)
		}
	}

	body := do(t, testServer(t, discardLogger()).handler(), http.MethodGet, "/", nil).Body.String()
	// Every /static URL in the shell has to carry the version: an unversioned
	// one would stay pinned to a file name a deploy never changes.
	for _, want := range []string{
		`/static/app.css?v=` + version,
		`/static/app.js?v=` + version,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("shell does not reference %q", want)
		}
	}
	if strings.Contains(body, `href="/static/app.css"`) || strings.Contains(body, `src="/static/app.js"`) {
		t.Error("shell references a static asset without the version query")
	}
}

func TestStaticBundleCoversEveryEmbeddedAsset(t *testing.T) {
	bundle := staticAssets()

	embedded := map[string]bool{}
	err := fs.WalkDir(web.FS, "static", func(p string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		embedded[strings.TrimPrefix(p, staticDirPrefix)] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded assets: %v", err)
	}

	if len(embedded) == 0 {
		t.Fatal("no embedded assets found: the walk is looking in the wrong directory")
	}
	for name := range embedded {
		asset, ok := bundle.files[name]
		if !ok {
			t.Errorf("embedded asset %q is missing from the bundle", name)
			continue
		}
		if len(asset.body) == 0 {
			t.Errorf("asset %q has an empty body", name)
		}
		if asset.contentType == "" {
			t.Errorf("asset %q has no content type", name)
		}
	}
	for name := range bundle.files {
		if !embedded[name] {
			t.Errorf("bundle serves %q, which is not embedded", name)
		}
	}

	// The compiler is a pure function of the embedded bytes, so memoising it
	// must not make the second caller see anything different.
	if again := compileStaticAssets(); again.version != bundle.version {
		t.Errorf("compileStaticAssets is not deterministic: %q then %q", bundle.version, again.version)
	}
}

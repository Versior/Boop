package server

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boop/internal/config"
	"boop/internal/media"
)

// ---------- helpers ----------

// pngFixture is a real PNG so the server sniffs it as image/png.
func pngFixture(t *testing.T, width, height int, variety byte) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.RGBA{R: variety, G: 200, B: 100, A: 255})
	if width > 1 && height > 1 {
		img.Set(width-1, height-1, color.RGBA{R: 10, G: variety, B: 90, A: 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

// uploadBody builds a multipart body with the given parts. Every part is a file
// part unless its name is empty, which sends a plain form field instead.
func uploadBody(t *testing.T, parts []uploadPart) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for _, part := range parts {
		if part.filename == "" {
			if err := writer.WriteField(part.field, part.content); err != nil {
				t.Fatalf("write field: %v", err)
			}
			continue
		}
		filePart, err := writer.CreateFormFile(part.field, part.filename)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := filePart.Write([]byte(part.content)); err != nil {
			t.Fatalf("write file part: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return body, writer.FormDataContentType()
}

type uploadPart struct {
	field    string
	filename string
	content  string
}

// paddedPNG returns a real PNG whose byte length is exactly size. Boop only
// reads the image header (sniff + dimensions), so trailing padding makes the
// length exact without encoding a megabyte of pixels in every test.
func paddedPNG(t *testing.T, size int) []byte {
	t.Helper()
	base := pngFixture(t, 8, 8, 3)
	if size < len(base) {
		t.Fatalf("paddedPNG(%d) is smaller than the base PNG", size)
	}
	padded := make([]byte, size)
	copy(padded, base)
	return padded
}

// uploadPNG uploads one image as the signed-in owner and returns the asset data.
func (c *contentFixture) uploadPNG(t *testing.T, filename string, data []byte) map[string]any {
	t.Helper()
	body, contentType := uploadBody(t, []uploadPart{{field: "file", filename: filename, content: string(data)}})
	rec := c.uploadPOSTFor(t, body, contentType, c.cookie, c.csrf, testOrigin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload %s: status = %d, body %s", filename, rec.Code, rec.Body.String())
	}
	return decodeData(t, rec)
}

// uploadRoot is where the fixture's configuration stores uploads.
func (c *contentFixture) uploadRoot() string {
	return filepath.Join(c.cfg.DataDir, media.UploadsDirName)
}

func (c *contentFixture) storedFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	root := c.uploadRoot()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !entry.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk uploads: %v", err)
	}
	return files
}

// ---------- authorization ----------

func TestUploadRequiresOwner(t *testing.T) {
	f := newAuthFixture(t)
	body, contentType := uploadBody(t, []uploadPart{{field: "file", filename: "sea.png", content: string(pngFixture(t, 4, 4, 1))}})

	t.Run("guest has no origin", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/uploads", bytes.NewReader(body.Bytes()))
		req.Header.Set("Content-Type", contentType)
		f.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "origin_required" {
			t.Errorf("code = %q, want origin_required", code)
		}
	})

	t.Run("guest with origin", func(t *testing.T) {
		rec := f.uploadPOSTFor(t, body, contentType, nil, "", testOrigin)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "unauthorized" {
			t.Errorf("code = %q, want unauthorized", code)
		}
	})

	t.Run("reader", func(t *testing.T) {
		owner := newContentFixture(t)
		readerCookie, readerCSRF := owner.readerFixture(t)
		rec := owner.uploadPOSTFor(t, body, contentType, readerCookie, readerCSRF, testOrigin)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "forbidden" {
			t.Errorf("code = %q, want forbidden", code)
		}
		if files := owner.storedFiles(t); len(files) != 0 {
			t.Errorf("a rejected upload wrote files: %v", files)
		}
		if assets := owner.countRows(t, "assets"); assets != 0 {
			t.Errorf("assets = %d, want none", assets)
		}
	})

	t.Run("owner without csrf", func(t *testing.T) {
		owner := newContentFixture(t)
		rec := owner.uploadPOSTFor(t, body, contentType, owner.cookie, "", testOrigin)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "csrf_invalid" {
			t.Errorf("code = %q, want csrf_invalid", code)
		}
	})

	t.Run("owner from another origin", func(t *testing.T) {
		owner := newContentFixture(t)
		rec := owner.uploadPOSTFor(t, body, contentType, owner.cookie, owner.csrf, "https://evil.example")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "origin_mismatch" {
			t.Errorf("code = %q, want origin_mismatch", code)
		}
	})
}

// uploadPOSTFor is the fixture-flavoured uploadPOST used before a content
// fixture exists.
func (f *authFixture) uploadPOSTFor(t *testing.T, body *bytes.Buffer, contentType string, cookie *http.Cookie, csrf, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/uploads", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", contentType)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// ---------- uploads in an object bucket ----------

// objectStorageConfig points uploads at a bucket, which is what turns the asset
// route into a redirect and every published address into a CDN address.
func objectStorageConfig(endpoint string) config.Config {
	cfg := testConfig()
	cfg.Storage = config.Storage{
		Endpoint:  endpoint,
		Region:    "auto",
		Bucket:    "boop-uploads",
		Prefix:    "uploads",
		PublicURL: "https://uploads.example.com",
		AccessKey: "an-access-key",
		SecretKey: "a-secret-key",
	}
	return cfg
}

// fakeObjectEndpoint accepts every write, which is all the server needs in order
// to finish an upload that does not go to disk.
func fakeObjectEndpoint(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestUploadPublishesTheBucketAddress(t *testing.T) {
	endpoint := fakeObjectEndpoint(t)
	c := newContentFixtureWithConfig(t, objectStorageConfig(endpoint.URL))

	asset := c.uploadPNG(t, "雾海.png", pngFixture(t, 8, 5, 13))
	key, ok := asset["storage_key"].(string)
	if !ok {
		t.Fatalf("storage_key missing from %v", asset)
	}
	want := "https://uploads.example.com/uploads/" + key
	if asset["url"] != want {
		t.Errorf("url = %v, want %q", asset["url"], want)
	}
	// Two copies that can disagree is worse than either one alone, so nothing
	// may be written under the data directory while the bucket is in charge.
	if files := c.storedFiles(t); len(files) != 0 {
		t.Errorf("local files were written alongside the object: %v", files)
	}

	rec := c.do(t, http.MethodGet, "/uploads/"+key, "", nil, nil)
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /uploads/%s: status = %d, want 301", key, rec.Code)
	}
	if got := rec.Header().Get("Location"); got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	if cache := rec.Header().Get("Cache-Control"); !strings.Contains(cache, "immutable") {
		t.Errorf("Cache-Control = %q, want the redirect to be cacheable too", cache)
	}
}

func TestAssetRouteStillRefusesKeysThatAddressNothing(t *testing.T) {
	// The redirect must not become a way to publish an arbitrary address. Only a
	// key with the generated shape leaves this process: every other request is
	// answered here, and none of them is answered with a bucket address.
	endpoint := fakeObjectEndpoint(t)
	c := newContentFixtureWithConfig(t, objectStorageConfig(endpoint.URL))

	targets := []string{
		"/uploads/nope.png",
		"/uploads/2026/09/not-hex.png",
		"/uploads/2026/09/0123456789abcdef0123456789abcdef.svg",
		"/uploads/../boop.db",
		"/uploads/%2e%2e%2fsecret.txt",
		"/uploads/..%2fsecret.txt",
		`/uploads/2026\09\0123456789abcdef0123456789abcdef.png`,
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			rec := c.do(t, http.MethodGet, target, "", nil, nil)
			if rec.Code == http.StatusOK {
				t.Fatalf("status = 200 for %s", target)
			}
			if location := rec.Header().Get("Location"); strings.Contains(location, "uploads.example.com") {
				t.Errorf("%s was answered with the bucket address %q", target, location)
			}
		})
	}
}

// ---------- accepted and rejected content ----------

func TestUploadStoresImageAndReturnsAsset(t *testing.T) {
	c := newContentFixture(t)
	data := pngFixture(t, 9, 6, 30)

	asset := c.uploadPNG(t, "海边.png", data)

	if asset["reused"] != false {
		t.Errorf("reused = %v, want false", asset["reused"])
	}
	if asset["mime_type"] != "image/png" {
		t.Errorf("mime_type = %v", asset["mime_type"])
	}
	if size, _ := asset["size_bytes"].(float64); int(size) != len(data) {
		t.Errorf("size_bytes = %v, want %d", asset["size_bytes"], len(data))
	}
	if width, _ := asset["width"].(float64); int(width) != 9 {
		t.Errorf("width = %v, want 9", asset["width"])
	}
	if height, _ := asset["height"].(float64); int(height) != 6 {
		t.Errorf("height = %v, want 6", asset["height"])
	}
	if asset["original_name"] != "海边.png" {
		t.Errorf("original_name = %v", asset["original_name"])
	}

	key, _ := asset["storage_key"].(string)
	url, _ := asset["url"].(string)
	if !media.ValidStorageKey(key) {
		t.Fatalf("storage_key = %q, want the generated shape", key)
	}
	if url != "/uploads/"+key {
		t.Errorf("url = %q, want /uploads/%s", url, key)
	}
	if !strings.HasSuffix(key, ".png") {
		t.Errorf("storage_key = %q, want the .png extension", key)
	}

	stored, err := os.ReadFile(filepath.Join(c.uploadRoot(), filepath.FromSlash(key)))
	if err != nil {
		t.Fatalf("read stored file: %v", err)
	}
	if !bytes.Equal(stored, data) {
		t.Error("stored bytes differ from the upload")
	}
	if assets := c.countRows(t, "assets"); assets != 1 {
		t.Errorf("assets = %d, want 1", assets)
	}
}

func TestUploadRejectsClientFilenameTraversal(t *testing.T) {
	c := newContentFixture(t)

	for index, filename := range []string{"../../evil.png", `..\..\evil.png`, "/etc/passwd.png", `C:\Windows\evil.png`} {
		t.Run(filename, func(t *testing.T) {
			// Distinct bytes per case, so a stored asset is genuinely new.
			asset := c.uploadPNG(t, filename, pngFixture(t, 3, 3, byte(50+index)))
			key, _ := asset["storage_key"].(string)
			if !media.ValidStorageKey(key) {
				t.Fatalf("storage_key = %q, want the generated shape", key)
			}
			if name, _ := asset["original_name"].(string); strings.ContainsAny(name, `/\`) {
				t.Errorf("original_name = %q keeps a separator", name)
			}
		})
	}

	for _, escaped := range []string{
		filepath.Join(c.cfg.DataDir, "evil.png"),
		filepath.Join(filepath.Dir(c.cfg.DataDir), "evil.png"),
		filepath.Join(c.cfg.DataDir, "passwd.png"),
	} {
		if _, err := os.Stat(escaped); err == nil {
			t.Errorf("an upload escaped the root: %s", escaped)
		}
	}
}

func TestUploadRejectsUnsupportedAndSpoofedFiles(t *testing.T) {
	c := newContentFixture(t)

	cases := []struct {
		name     string
		filename string
		content  string
		status   int
		code     string
	}{
		{"html payload", "page.png", "<!doctype html><html><body>hi</body></html>", http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"script payload", "page.png", "<script>alert(1)</script>", http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"svg payload", "icon.svg", `<svg xmlns="http://www.w3.org/2000/svg" width="8" height="8"></svg>`, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"png named jpg", "photo.jpg", string(pngFixture(t, 4, 4, 4)), http.StatusBadRequest, "invalid_filename"},
		{"no extension", "photo", string(pngFixture(t, 4, 4, 5)), http.StatusBadRequest, "invalid_filename"},
		{"double extension", "photo.png.exe", string(pngFixture(t, 4, 4, 6)), http.StatusBadRequest, "invalid_filename"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, contentType := uploadBody(t, []uploadPart{{field: "file", filename: tc.filename, content: tc.content}})
			rec := c.uploadPOSTFor(t, body, contentType, c.cookie, c.csrf, testOrigin)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			if code, _, _ := decodeAPIError(t, rec); code != tc.code {
				t.Errorf("code = %q, want %q", code, tc.code)
			}
		})
	}

	if files := c.storedFiles(t); len(files) != 0 {
		t.Errorf("rejected uploads wrote files: %v", files)
	}
	if assets := c.countRows(t, "assets"); assets != 0 {
		t.Errorf("assets = %d, want none", assets)
	}
}

func TestUploadRejectsBadMultipartShape(t *testing.T) {
	c := newContentFixture(t)

	t.Run("not multipart", func(t *testing.T) {
		rec := c.post(t, "/api/v1/admin/uploads", `{"file":"nope"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "invalid_body" {
			t.Errorf("code = %q, want invalid_body", code)
		}
	})

	t.Run("no file part", func(t *testing.T) {
		body, contentType := uploadBody(t, []uploadPart{{field: "note", content: "只有文字"}})
		rec := c.uploadPOSTFor(t, body, contentType, c.cookie, c.csrf, testOrigin)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "invalid_body" {
			t.Errorf("code = %q, want invalid_body", code)
		}
	})

	t.Run("two file parts", func(t *testing.T) {
		body, contentType := uploadBody(t, []uploadPart{
			{field: "file", filename: "one.png", content: string(pngFixture(t, 3, 3, 8))},
			{field: "file", filename: "two.png", content: string(pngFixture(t, 3, 3, 9))},
		})
		rec := c.uploadPOSTFor(t, body, contentType, c.cookie, c.csrf, testOrigin)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "invalid_body" {
			t.Errorf("code = %q, want invalid_body", code)
		}
		if assets := c.countRows(t, "assets"); assets != 0 {
			t.Errorf("assets = %d, want none", assets)
		}
	})

	t.Run("wrong method", func(t *testing.T) {
		rec := c.do(t, http.MethodGet, "/api/v1/admin/uploads", "", nil, c.cookie)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405: %s", rec.Code, rec.Body.String())
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Errorf("Allow = %q, want POST", allow)
		}
	})
}

// The documented ceiling is a single-file ceiling: multipart framing must not
// push a file of exactly BOOP_MAX_UPLOAD_MB over the limit, while the request
// body still has a bounded total cap.
func TestUploadSizeBoundaries(t *testing.T) {
	const mebibyte = 1 << 20

	newFixture := func(t *testing.T) (*contentFixture, *http.Cookie, string) {
		t.Helper()
		cfg := testConfig()
		cfg.MaxUploadMB = 1
		cfg.DataDir = t.TempDir()
		f := newAuthFixtureWithConfig(t, cfg)
		f.bootstrapOwner(t, "owner@example.com", "遇事开心")
		rec := f.login(t, "owner@example.com", authPassword, nil)
		return &contentFixture{authFixture: f}, sessionCookie(t, rec), decodeData(t, rec)["csrf_token"].(string)
	}

	t.Run("a file of exactly the limit is accepted", func(t *testing.T) {
		c, cookie, csrf := newFixture(t)
		body, contentType := uploadBody(t, []uploadPart{{field: "file", filename: "exact.png", content: string(paddedPNG(t, mebibyte))}})
		rec := c.uploadPOSTFor(t, body, contentType, cookie, csrf, testOrigin)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 for a file of exactly the limit: %s", rec.Code, rec.Body.String())
		}
		asset := decodeData(t, rec)
		if size, _ := asset["size_bytes"].(float64); int64(size) != mebibyte {
			t.Errorf("size_bytes = %v, want %d", asset["size_bytes"], mebibyte)
		}
		files := c.storedFiles(t)
		if len(files) != 1 {
			t.Fatalf("files = %v, want one stored file", files)
		}
		info, err := os.Stat(files[0])
		if err != nil {
			t.Fatalf("stat stored file: %v", err)
		}
		if info.Size() != mebibyte {
			t.Errorf("stored file = %d bytes, want %d", info.Size(), mebibyte)
		}
	})

	t.Run("one byte over the limit is rejected", func(t *testing.T) {
		c, cookie, csrf := newFixture(t)
		body, contentType := uploadBody(t, []uploadPart{{field: "file", filename: "over.png", content: string(paddedPNG(t, mebibyte+1))}})
		rec := c.uploadPOSTFor(t, body, contentType, cookie, csrf, testOrigin)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413: %s", rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "payload_too_large" {
			t.Errorf("code = %q, want payload_too_large", code)
		}
		assertNoUploadRuins(t, c)
	})

	t.Run("the framing budget is bounded", func(t *testing.T) {
		c, cookie, csrf := newFixture(t)
		// A legal file plus an oversized extra field must still hit a total cap.
		body, contentType := uploadBody(t, []uploadPart{
			{field: "file", filename: "exact.png", content: string(paddedPNG(t, mebibyte))},
			{field: "padding", content: strings.Repeat("y", int(multipartBodyOverhead)+1<<10)},
		})
		rec := c.uploadPOSTFor(t, body, contentType, cookie, csrf, testOrigin)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413 for a body past the framing budget: %s", rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "payload_too_large" {
			t.Errorf("code = %q, want payload_too_large", code)
		}
		assertNoUploadRuins(t, c)
	})
}

// assertNoUploadRuins checks that a rejected upload stored neither a row nor a
// file under the upload root.
func assertNoUploadRuins(t *testing.T, c *contentFixture) {
	t.Helper()
	if files := c.storedFiles(t); len(files) != 0 {
		t.Errorf("files = %v, want none", files)
	}
	if assets := c.countRows(t, "assets"); assets != 0 {
		t.Errorf("assets = %d, want none", assets)
	}
}

func TestUploadRejectsOversizedRequest(t *testing.T) {
	cfg := testConfig()
	cfg.MaxUploadMB = 1
	cfg.DataDir = t.TempDir()
	f := newAuthFixtureWithConfig(t, cfg)
	f.bootstrapOwner(t, "owner@example.com", "遇事开心")
	rec := f.login(t, "owner@example.com", authPassword, nil)
	cookie := sessionCookie(t, rec)
	csrf := decodeData(t, rec)["csrf_token"].(string)

	// One mebibyte of PNG plus multipart framing exceeds the one mebibyte file
	// budget.
	big := append(pngFixture(t, 8, 8, 11), bytes.Repeat([]byte{0}, 1<<20)...)
	body, contentType := uploadBody(t, []uploadPart{{field: "file", filename: "big.png", content: string(big)}})
	up := f.uploadPOSTFor(t, body, contentType, cookie, csrf, testOrigin)
	if up.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", up.Code, up.Body.String())
	}
	if code, _, _ := decodeAPIError(t, up); code != "payload_too_large" {
		t.Errorf("code = %q, want payload_too_large", code)
	}

	root := filepath.Join(cfg.DataDir, media.UploadsDirName)
	if entries, err := os.ReadDir(root); err == nil && len(entries) != 0 {
		t.Errorf("oversized upload left %d entries under %s", len(entries), root)
	}
	var assets int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM assets`).Scan(&assets); err != nil {
		t.Fatalf("count assets: %v", err)
	}
	if assets != 0 {
		t.Errorf("assets = %d, want none", assets)
	}
}

// The API documents exactly one file field named "file": a file part under any
// other name is a contract violation, while plain form fields stay ignored.
func TestUploadRequiresTheFileFieldName(t *testing.T) {
	c := newContentFixture(t)
	data := pngFixture(t, 6, 6, 61)

	t.Run("wrong file field name", func(t *testing.T) {
		body, contentType := uploadBody(t, []uploadPart{{field: "image", filename: "sea.png", content: string(data)}})
		rec := c.uploadPOSTFor(t, body, contentType, c.cookie, c.csrf, testOrigin)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
		code, _, _ := decodeAPIError(t, rec)
		if code != "invalid_body" {
			t.Errorf("code = %q, want invalid_body", code)
		}
		if assets := c.countRows(t, "assets"); assets != 0 {
			t.Errorf("assets = %d, want none for a misnamed field", assets)
		}
		if files := c.storedFiles(t); len(files) != 0 {
			t.Errorf("files = %v, want none for a misnamed field", files)
		}
	})

	t.Run("extra non-file fields stay ignored", func(t *testing.T) {
		body, contentType := uploadBody(t, []uploadPart{
			{field: "file", filename: "sea.png", content: string(data)},
			{field: "caption", content: "雾里的海岸线"},
		})
		rec := c.uploadPOSTFor(t, body, contentType, c.cookie, c.csrf, testOrigin)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestUploadReusesIdenticalBytes(t *testing.T) {
	c := newContentFixture(t)
	data := pngFixture(t, 5, 5, 12)

	first := c.uploadPNG(t, "sea.png", data)

	body, contentType := uploadBody(t, []uploadPart{{field: "file", filename: "again.png", content: string(data)}})
	rec := c.uploadPOSTFor(t, body, contentType, c.cookie, c.csrf, testOrigin)
	if rec.Code != http.StatusOK {
		t.Fatalf("duplicate status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	second := decodeData(t, rec)

	if second["reused"] != true {
		t.Errorf("reused = %v, want true", second["reused"])
	}
	if second["id"] != first["id"] || second["storage_key"] != first["storage_key"] {
		t.Errorf("duplicate upload = %v, want the existing asset %v", second, first)
	}
	if assets := c.countRows(t, "assets"); assets != 1 {
		t.Errorf("assets = %d, want 1", assets)
	}
	if files := c.storedFiles(t); len(files) != 1 {
		t.Errorf("files = %v, want one stored file", files)
	}
}

// ---------- public serving ----------

func TestUploadsAreServedPublicly(t *testing.T) {
	c := newContentFixture(t)
	data := pngFixture(t, 7, 7, 21)
	asset := c.uploadPNG(t, "海.png", data)
	key, _ := asset["storage_key"].(string)

	rec := c.do(t, http.MethodGet, "/uploads/"+key, "", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an anonymous read: %s", rec.Code, rec.Body.String())
	}
	if contentType := rec.Header().Get("Content-Type"); contentType != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", contentType)
	}
	if nosniff := rec.Header().Get("X-Content-Type-Options"); nosniff != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", nosniff)
	}
	if cache := rec.Header().Get("Cache-Control"); !strings.Contains(cache, "max-age=31536000") || !strings.Contains(cache, "immutable") {
		t.Errorf("Cache-Control = %q, want a long immutable cache", cache)
	}
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Error("served bytes differ from the upload")
	}

	t.Run("missing key", func(t *testing.T) {
		rec := c.do(t, http.MethodGet, "/uploads/2026/09/ffffffffffffffffffffffffffffffff.png", "", nil, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("head request", func(t *testing.T) {
		rec := c.do(t, http.MethodHead, "/uploads/"+key, "", nil, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})
}

func TestUploadsRejectTraversal(t *testing.T) {
	c := newContentFixture(t)
	secret := "boop-data-directory-secret"
	if err := os.WriteFile(filepath.Join(c.cfg.DataDir, "secret.txt"), []byte(secret), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	targets := []string{
		"/uploads/../secret.txt",
		"/uploads/../../etc/passwd",
		"/uploads/%2e%2e%2fsecret.txt",
		"/uploads/..%2fsecret.txt",
		"/uploads//etc/passwd",
		"/uploads/2026/09/../../secret.txt",
		"/uploads/2026/09/0123456789abcdef0123456789abcdef.png/../../../secret.txt",
		`/uploads/2026\09\0123456789abcdef0123456789abcdef.png`,
		"/uploads/2026/09/0123456789abcdef0123456789abcdef.svg",
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			rec := c.do(t, http.MethodGet, target, "", nil, nil)
			if rec.Code == http.StatusOK {
				t.Fatalf("status = 200 for %s: %s", target, rec.Body.String())
			}
			body := rec.Body.String()
			if strings.Contains(body, secret) {
				t.Fatalf("%s served a file outside the upload root", target)
			}
			if strings.Contains(body, "schema_migrations") {
				t.Fatalf("%s served the database", target)
			}
		})
	}
}

// ---------- publishing a photo with a real upload ----------

func TestPhotoPublishUsesUploadedAsset(t *testing.T) {
	c := newContentFixture(t)
	asset := c.uploadPNG(t, "雾海.png", pngFixture(t, 8, 5, 31))
	assetID := int64(asset["id"].(float64))

	created := c.createOK(t, map[string]any{
		"type": "photo", "status": "published", "body": "雾里的海岸线",
		"asset_ids": []int64{assetID}, "location": "青岛 · 石老人海边",
	})
	if cover, _ := created["url"].(string); cover == "" {
		t.Errorf("created photo has no url: %v", created)
	}
	assets, ok := created["assets"].([]any)
	if !ok || len(assets) != 1 {
		t.Fatalf("assets = %v, want the uploaded image", created["assets"])
	}
	if got := assets[0].(map[string]any)["url"]; got != "/uploads/"+asset["storage_key"].(string) {
		t.Errorf("asset url = %v", got)
	}

	slug, _ := created["slug"].(string)
	page := c.do(t, http.MethodGet, "/p/"+slug, "", nil, nil)
	if page.Code != http.StatusOK {
		t.Fatalf("detail page status = %d", page.Code)
	}
	if body := page.Body.String(); !strings.Contains(body, "/uploads/"+asset["storage_key"].(string)) {
		t.Errorf("detail page does not reference the uploaded image")
	}

	// The public home feed must show the same upload.
	home := c.do(t, http.MethodGet, "/", "", nil, nil).Body.String()
	if !strings.Contains(home, "/uploads/"+asset["storage_key"].(string)) {
		t.Errorf("home feed does not reference the uploaded image")
	}
	image := c.do(t, http.MethodGet, "/uploads/"+asset["storage_key"].(string), "", nil, nil)
	if image.Code != http.StatusOK {
		t.Errorf("public image status = %d, want 200 without a session", image.Code)
	}
}

func TestPhotoPublishRejectsUnusableAssets(t *testing.T) {
	c := newContentFixture(t)
	if rec := c.register(t, "reader@example.com", "读者甲"); rec.Code != http.StatusCreated {
		t.Fatalf("register reader: status = %d, body %s", rec.Code, rec.Body.String())
	}

	// A reader's asset row cannot exist through the API; it stands in for any
	// row the writer does not own.
	res, err := c.db.Exec(
		`INSERT INTO assets(owner_user_id, storage_key, original_name, mime_type, size_bytes, sha256, created_at)
		 VALUES((SELECT id FROM users WHERE email = 'reader@example.com'),'2026/09/ffffffffffffffffffffffffffffffff.jpg','x.jpg','image/jpeg',10,x'bb',?)`,
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert foreign asset: %v", err)
	}
	foreignID, _ := res.LastInsertId()

	document, err := c.db.Exec(
		`INSERT INTO assets(owner_user_id, storage_key, original_name, mime_type, size_bytes, sha256, created_at)
		 VALUES(?, '2026/09/eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee.pdf','notes.pdf','application/pdf',10,x'cc',?)`,
		c.owner.ID, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert non-image asset: %v", err)
	}
	documentID, _ := document.LastInsertId()

	cases := []struct {
		name string
		ids  []int64
	}{
		{"foreign asset", []int64{foreignID}},
		{"non-image asset", []int64{documentID}},
		{"no asset", []int64{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"type": "photo", "status": "published", "body": "没有可用图片"}
			if len(tc.ids) > 0 {
				payload["asset_ids"] = tc.ids
			}
			rec := c.createPost(t, payload)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			code, _, _ := decodeAPIError(t, rec)
			want := "invalid_asset"
			if len(tc.ids) == 0 {
				want = "invalid_assets"
			}
			if code != want {
				t.Errorf("code = %q, want %q", code, want)
			}
		})
	}
	if posts := c.countRows(t, "posts"); posts != 0 {
		t.Errorf("posts = %d, want no rejected photo", posts)
	}
}

// The JSON content ceiling stays independent from the upload budget: a body far
// below BOOP_MAX_UPLOAD_MB is still rejected by its own limit.
func TestUploadBudgetIsIndependentFromJSONLimits(t *testing.T) {
	c := newContentFixture(t)

	rec := c.post(t, "/api/v1/admin/posts", `{"body":"`+strings.Repeat("x", 600<<10)+`"}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want the 512KiB content ceiling: %s", rec.Code, rec.Body.String())
	}
	if code, _, _ := decodeAPIError(t, rec); code != "payload_too_large" {
		t.Errorf("code = %q, want payload_too_large", code)
	}
}

// The upload response is a stable contract the composer depends on.
func TestUploadResponseShape(t *testing.T) {
	c := newContentFixture(t)
	asset := c.uploadPNG(t, "sea.png", pngFixture(t, 4, 4, 41))

	encoded, err := json.Marshal(asset)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"id", "url", "storage_key", "mime_type", "size_bytes", "width", "height", "original_name", "reused"} {
		if !strings.Contains(string(encoded), `"`+key+`"`) {
			t.Errorf("upload payload lacks %q: %s", key, encoded)
		}
	}
}

// The composer is wired to the real upload endpoint through the shipped script:
// nothing in the SSR DOM can prove the fetch target on its own.
func TestComposerScriptTargetsTheUploadEndpoint(t *testing.T) {
	c := newContentFixture(t)

	script := c.do(t, http.MethodGet, "/static/app.js", "", nil, nil)
	if script.Code != http.StatusOK {
		t.Fatalf("static script status = %d", script.Code)
	}
	body := script.Body.String()
	for _, marker := range []string{"/api/v1/admin/uploads", "FormData", "data-composer-file", "asset_ids"} {
		if !strings.Contains(body, marker) {
			t.Errorf("app.js is missing %q, so photo publishing is not wired to uploads", marker)
		}
	}
}

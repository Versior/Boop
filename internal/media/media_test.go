package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boop/internal/store"
)

var testNow = time.Date(2026, 9, 28, 6, 0, 0, 0, time.UTC)

// testOptions points the upload root at a temp directory so no test can write
// into the repository or the real data directory.
func testOptions(t *testing.T) Options {
	t.Helper()
	return Options{DataDir: t.TempDir(), MaxBytes: 4 << 20}
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "boop.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(db); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	return db
}

// insertUser adds an account so asset owner isolation can be exercised.
func insertUser(t *testing.T, db *sql.DB, email string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO users(email, password_hash, display_name, role, status, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?)`,
		email, "x", "遇事开心", "owner", "active",
		testNow.Format(time.RFC3339), testNow.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("user id: %v", err)
	}
	return id
}

func pngBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func jpegBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

func gifBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewPaletted(image.Rect(0, 0, width, height), color.Palette{color.Black, color.White})
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatalf("gif.Encode: %v", err)
	}
	return buf.Bytes()
}

// webpBytes builds a minimal RIFF/WEBP container with the requested variant
// chunk: VP8X (extended), VP8L (lossless) or VP8 (lossy).
func webpBytes(variant string, width, height int) []byte {
	out := []byte("RIFF\x00\x00\x00\x00WEBP" + variant)
	switch variant {
	case "VP8X":
		out = append(out, 0, 0, 0, 0) // chunk size, ignored by the parser
		out = append(out, 0, 0, 0, 0) // flags plus reserved
		out = append(out, le24(width-1)...)
		out = append(out, le24(height-1)...)
	case "VP8L":
		out = append(out, 0, 0, 0, 0)
		out = append(out, 0x2f)
		bits := uint32(width-1) | uint32(height-1)<<14
		out = binary.LittleEndian.AppendUint32(out, bits)
	case "VP8 ":
		out = append(out, 0, 0, 0, 0)
		out = append(out, 0, 0, 0)          // frame tag
		out = append(out, 0x9d, 0x01, 0x2a) // sync code
		out = binary.LittleEndian.AppendUint16(out, uint16(width)&0x3fff)
		out = binary.LittleEndian.AppendUint16(out, uint16(height)&0x3fff)
	}
	return out
}

func le24(value int) []byte {
	return []byte{byte(value), byte(value >> 8), byte(value >> 16)}
}

// filesUnder lists every regular file under root, so tests can prove that
// nothing leaked outside the upload directory and that reuse stores one file.
func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
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
		t.Fatalf("walk %s: %v", root, err)
	}
	return files
}

// ---------- sniffing and validation ----------

func TestIsImageMIME(t *testing.T) {
	cases := map[string]bool{
		"image/jpeg":               true,
		"image/png":                true,
		"image/webp":               true,
		"image/gif":                true,
		"image/svg+xml":            false,
		"text/html":                false,
		"application/octet-stream": false,
		"":                         false,
	}
	for mime, want := range cases {
		if got := IsImageMIME(mime); got != want {
			t.Errorf("IsImageMIME(%q) = %v, want %v", mime, got, want)
		}
	}
}

func TestValidStorageKey(t *testing.T) {
	const key = "2026/09/0123456789abcdef0123456789abcdef.png"
	valid := []string{key, "2026/09/0123456789abcdef0123456789abcdef.jpg", "2026/09/0123456789abcdef0123456789abcdef.webp"}
	for _, candidate := range valid {
		if !ValidStorageKey(candidate) {
			t.Errorf("ValidStorageKey(%q) = false, want true", candidate)
		}
	}
	invalid := []string{
		"", "2026/09/0123456789abcdef0123456789abcdef.svg",
		"2026/09/0123456789abcdef0123456789abcdef", "2026/09/abc.png",
		"2026/09/0123456789ABCDEF0123456789ABCDEF.png",
		"../boop.db", "2026/09/../../boop.db", "2026/09/0123456789abcdef0123456789abcdef.png/../x",
		"/etc/passwd", `2026\09\0123456789abcdef0123456789abcdef.png`,
		"2026/09/0123456789abcdef0123456789abcdef.png.exe",
	}
	for _, candidate := range invalid {
		if ValidStorageKey(candidate) {
			t.Errorf("ValidStorageKey(%q) = true, want false", candidate)
		}
	}
}

func TestDimensionsPerFormat(t *testing.T) {
	cases := []struct {
		name   string
		file   string
		data   []byte
		width  int
		height int
	}{
		{"png", "a.png", pngBytes(t, 7, 5), 7, 5},
		{"jpeg", "a.jpg", jpegBytes(t, 11, 3), 11, 3},
		{"gif", "a.gif", gifBytes(t, 4, 9), 4, 9},
		{"webp vp8x", "a.webp", webpBytes("VP8X", 13, 17), 13, 17},
		{"webp vp8l", "a.webp", webpBytes("VP8L", 21, 33), 21, 33},
		{"webp vp8 ", "a.webp", webpBytes("VP8 ", 9, 15), 9, 15},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mime, _, err := sniff(tc.file, tc.data)
			if err != nil {
				t.Fatalf("sniff(%s): %v", tc.file, err)
			}
			width, height, ok := dimensions(mime, tc.data)
			if !ok {
				t.Fatalf("dimensions(%s) failed", mime)
			}
			if width != tc.width || height != tc.height {
				t.Errorf("dimensions = %dx%d, want %dx%d", width, height, tc.width, tc.height)
			}
		})
	}

	t.Run("truncated data is unknown", func(t *testing.T) {
		if _, _, ok := dimensions("image/webp", webpBytes("VP8X", 4, 4)[:20]); ok {
			t.Error("truncated webp reported dimensions")
		}
		if _, _, ok := dimensions("image/png", pngBytes(t, 4, 4)[:10]); ok {
			t.Error("truncated png reported dimensions")
		}
	})
}

// ---------- storing ----------

func TestStoreWritesImageUnderDatePath(t *testing.T) {
	opts := testOptions(t)
	db := testDB(t)
	owner := insertUser(t, db, "owner@example.com")
	data := pngBytes(t, 7, 5)

	asset, err := Store(context.Background(), db, opts, owner, "海滩.png", data, testNow)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	if asset.Reused {
		t.Error("first upload reported reuse")
	}
	if asset.MimeType != "image/png" {
		t.Errorf("mime = %q, want image/png", asset.MimeType)
	}
	if asset.SizeBytes != int64(len(data)) {
		t.Errorf("size = %d, want %d", asset.SizeBytes, len(data))
	}
	if asset.Width == nil || *asset.Width != 7 || asset.Height == nil || *asset.Height != 5 {
		t.Errorf("dimensions = %v x %v, want 7x5", asset.Width, asset.Height)
	}
	if asset.OriginalName != "海滩.png" {
		t.Errorf("original_name = %q", asset.OriginalName)
	}
	wantSHA := sha256.Sum256(data)
	if !bytes.Equal(asset.SHA256[:], wantSHA[:]) {
		t.Error("stored sha256 does not match the bytes")
	}
	if !strings.HasPrefix(asset.StorageKey, "2026/09/") {
		t.Errorf("storage key %q is not under the upload month", asset.StorageKey)
	}
	if !ValidStorageKey(asset.StorageKey) {
		t.Errorf("storage key %q does not match the public key shape", asset.StorageKey)
	}
	if !strings.HasSuffix(asset.StorageKey, ".png") {
		t.Errorf("storage key %q kept the wrong extension", asset.StorageKey)
	}

	stored, err := os.ReadFile(filepath.Join(opts.DataDir, UploadsDirName, filepath.FromSlash(asset.StorageKey)))
	if err != nil {
		t.Fatalf("read stored file: %v", err)
	}
	if !bytes.Equal(stored, data) {
		t.Error("stored bytes differ from the upload")
	}

	var mime, name, createdAt string
	var size int64
	if err := db.QueryRow(
		`SELECT mime_type, original_name, size_bytes, created_at FROM assets WHERE id = ?`, asset.ID,
	).Scan(&mime, &name, &size, &createdAt); err != nil {
		t.Fatalf("read asset row: %v", err)
	}
	if mime != "image/png" || name != "海滩.png" || size != int64(len(data)) {
		t.Errorf("row = %q %q %d", mime, name, size)
	}
	if _, err := time.Parse(time.RFC3339, createdAt); err != nil {
		t.Errorf("created_at %q is not RFC3339: %v", createdAt, err)
	}
}

func TestStoreRejectsUnsupportedContent(t *testing.T) {
	opts := testOptions(t)
	db := testDB(t)
	owner := insertUser(t, db, "owner@example.com")

	cases := map[string][]byte{
		"html":        []byte("<!doctype html><html><body>hi</body></html>"),
		"script":      []byte("<script>alert(1)</script>"),
		"svg":         []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="8" height="8"></svg>`),
		"plain text":  []byte("这不是图片，只是文字。"),
		"empty":       {},
		"svg content": []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"></svg>`),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Store(context.Background(), db, opts, owner, "evil.png", data, testNow); !errors.Is(err, ErrUnsupportedMedia) {
				t.Fatalf("error = %v, want ErrUnsupportedMedia", err)
			}
		})
	}
	if files := filesUnder(t, opts.DataDir); len(files) != 0 {
		t.Errorf("rejected uploads left files: %v", files)
	}
}

func TestStoreRejectsSpoofedExtension(t *testing.T) {
	opts := testOptions(t)
	db := testDB(t)
	owner := insertUser(t, db, "owner@example.com")
	png := pngBytes(t, 4, 4)

	spoofed := []string{"photo.jpg", "photo.jpeg", "photo.gif", "photo.webp", "photo.txt", "photo", "photo.png.exe", "photo.PNG.exe"}
	for _, name := range spoofed {
		t.Run(name, func(t *testing.T) {
			if _, err := Store(context.Background(), db, opts, owner, name, png, testNow); !errors.Is(err, ErrInvalidFilename) {
				t.Fatalf("error = %v, want ErrInvalidFilename", err)
			}
		})
	}

	if _, err := Store(context.Background(), db, opts, owner, "photo.PNG", png, testNow); err != nil {
		t.Errorf("uppercase extension rejected: %v", err)
	}
}

func TestStoreRejectsOversizedFile(t *testing.T) {
	opts := testOptions(t)
	opts.MaxBytes = 64
	db := testDB(t)
	owner := insertUser(t, db, "owner@example.com")

	if _, err := Store(context.Background(), db, opts, owner, "big.png", pngBytes(t, 64, 64), testNow); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error = %v, want ErrTooLarge", err)
	}
	if files := filesUnder(t, opts.DataDir); len(files) != 0 {
		t.Errorf("oversized upload left files: %v", files)
	}
	if count := countAssets(t, db); count != 0 {
		t.Errorf("assets = %d, want 0", count)
	}
}

func TestStoreReusesIdenticalBytes(t *testing.T) {
	opts := testOptions(t)
	db := testDB(t)
	owner := insertUser(t, db, "owner@example.com")
	data := jpegBytes(t, 6, 6)

	first, err := Store(context.Background(), db, opts, owner, "a.jpg", data, testNow)
	if err != nil {
		t.Fatalf("first Store: %v", err)
	}
	second, err := Store(context.Background(), db, opts, owner, "b.jpg", data, testNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("second Store: %v", err)
	}

	if second.Reused != true {
		t.Error("duplicate upload did not report reuse")
	}
	if second.ID != first.ID || second.StorageKey != first.StorageKey {
		t.Errorf("duplicate upload stored a new asset: %+v vs %+v", second, first)
	}
	if second.OriginalName != first.OriginalName {
		t.Errorf("reuse rewrote original_name to %q", second.OriginalName)
	}
	if count := countAssets(t, db); count != 1 {
		t.Errorf("assets = %d, want a single row for identical bytes", count)
	}
	if files := filesUnder(t, opts.DataDir); len(files) != 1 {
		t.Errorf("files = %v, want exactly one stored file", files)
	}
}

func TestStoreDeduplicatesPerOwner(t *testing.T) {
	opts := testOptions(t)
	db := testDB(t)
	first := insertUser(t, db, "owner@example.com")
	second := insertUser(t, db, "other@example.com")
	data := pngBytes(t, 5, 5)

	one, err := Store(context.Background(), db, opts, first, "a.png", data, testNow)
	if err != nil {
		t.Fatalf("Store owner one: %v", err)
	}
	two, err := Store(context.Background(), db, opts, second, "a.png", data, testNow)
	if err != nil {
		t.Fatalf("Store owner two: %v", err)
	}

	if two.ID == one.ID {
		t.Error("a second owner reused another owner's asset row")
	}
	if two.StorageKey == one.StorageKey {
		t.Error("a second owner reused another owner's file")
	}
	if count := countAssets(t, db); count != 2 {
		t.Errorf("assets = %d, want one per owner", count)
	}
}

func TestStoreKeepsClientFilenameOutOfPaths(t *testing.T) {
	opts := testOptions(t)
	db := testDB(t)
	owner := insertUser(t, db, "owner@example.com")

	names := []string{
		"../../evil.png",
		`..\..\evil.png`,
		"/etc/passwd.png",
		`C:\Windows\evil.png`,
		"nested/dir/photo.png",
		"..%2f..%2fevil.png",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			asset, err := Store(context.Background(), db, opts, owner, name, pngBytes(t, 3, 3), testNow)
			if err != nil {
				t.Fatalf("Store(%q): %v", name, err)
			}
			if !ValidStorageKey(asset.StorageKey) {
				t.Fatalf("storage key %q is not the generated shape", asset.StorageKey)
			}
			absolute := filepath.Join(opts.DataDir, UploadsDirName, filepath.FromSlash(asset.StorageKey))
			root := filepath.Join(opts.DataDir, UploadsDirName)
			if !strings.HasPrefix(absolute, root+string(os.PathSeparator)) {
				t.Fatalf("stored path %q escaped the upload root %q", absolute, root)
			}
			if strings.ContainsAny(asset.OriginalName, `/\`) {
				t.Errorf("original_name %q kept a path separator", asset.OriginalName)
			}
			if asset.OriginalName != "evil.png" && asset.OriginalName != "photo.png" {
				t.Errorf("original_name = %q, want only the base name", asset.OriginalName)
			}
		})
	}

	for _, escaped := range []string{
		filepath.Join(opts.DataDir, "evil.png"),
		filepath.Join(filepath.Dir(opts.DataDir), "evil.png"),
	} {
		if _, err := os.Stat(escaped); err == nil {
			t.Errorf("a file escaped the upload root: %s", escaped)
		}
	}
}

func TestStoreRejectsMissingOwner(t *testing.T) {
	opts := testOptions(t)
	db := testDB(t)

	if _, err := Store(context.Background(), db, opts, 0, "a.png", pngBytes(t, 4, 4), testNow); err == nil {
		t.Error("Store accepted an upload without an owner")
	}
	if _, err := Store(context.Background(), nil, opts, 1, "a.png", pngBytes(t, 4, 4), testNow); err == nil {
		t.Error("Store accepted a nil database")
	}
	if files := filesUnder(t, opts.DataDir); len(files) != 0 {
		t.Errorf("rejected uploads left files: %v", files)
	}
}

func TestStoreRemovesFileWhenInsertFails(t *testing.T) {
	opts := testOptions(t)
	db := testDB(t)
	owner := insertUser(t, db, "owner@example.com")

	if _, err := db.Exec(`CREATE TRIGGER assets_block BEFORE INSERT ON assets BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	if _, err := Store(context.Background(), db, opts, owner, "a.png", pngBytes(t, 4, 4), testNow); err == nil {
		t.Fatal("Store succeeded although the insert was blocked")
	}
	if files := filesUnder(t, opts.DataDir); len(files) != 0 {
		t.Errorf("failed insert left dirty files: %v", files)
	}
}

func TestStoreRemovesFileWhenDatabaseIsGone(t *testing.T) {
	opts := testOptions(t)
	db := testDB(t)
	owner := insertUser(t, db, "owner@example.com")
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	if _, err := Store(context.Background(), db, opts, owner, "a.png", pngBytes(t, 4, 4), testNow); err == nil {
		t.Fatal("Store succeeded on a closed database")
	}
	if files := filesUnder(t, opts.DataDir); len(files) != 0 {
		t.Errorf("closed database left files: %v", files)
	}
}

// TestInsertOrReuseCoversTheRaceBranch covers the branch a concurrent upload of
// the same bytes hits: the row appears between the pre-check and the insert, and
// insertOrReuse must hand back the existing row rather than create a duplicate.
func TestInsertOrReuseCoversTheRaceBranch(t *testing.T) {
	opts := testOptions(t)
	db := testDB(t)
	owner := insertUser(t, db, "owner@example.com")
	data := pngBytes(t, 4, 4)
	digest := sha256.Sum256(data)

	existing, err := Store(context.Background(), db, opts, owner, "first.png", data, testNow)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	duplicate, reused, err := insertOrReuse(context.Background(), db, assetRow{
		OwnerID:      owner,
		StorageKey:   "2026/09/ffffffffffffffffffffffffffffffff.png",
		OriginalName: "second.png",
		MimeType:     "image/png",
		SizeBytes:    int64(len(data)),
		SHA256:       digest,
		CreatedAt:    testNow.Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("insertOrReuse: %v", err)
	}
	if duplicate == nil || duplicate.ID != existing.ID {
		t.Fatalf("insertOrReuse created a duplicate instead of reusing: %+v", duplicate)
	}
	if !reused {
		t.Error("insertOrReuse did not report the reuse")
	}
	if count := countAssets(t, db); count != 1 {
		t.Errorf("assets = %d, want one row", count)
	}
}

// ---------- serving ----------

func TestOpenResolvesOnlyGeneratedKeys(t *testing.T) {
	opts := testOptions(t)
	db := testDB(t)
	owner := insertUser(t, db, "owner@example.com")

	asset, err := Store(context.Background(), db, opts, owner, "海滩.png", pngBytes(t, 4, 4), testNow)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	file, info, err := Open(opts, asset.StorageKey)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer file.Close()
	if info.Size() != int64(len(pngBytes(t, 4, 4))) {
		t.Errorf("served size = %d", info.Size())
	}

	// A database file next to the upload root must never be reachable.
	if err := os.WriteFile(filepath.Join(opts.DataDir, "boop.db"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	for _, key := range []string{
		"../boop.db", "2026/09/../../boop.db", "/etc/passwd", "..\\boop.db",
		"2026/09/0123456789abcdef0123456789abcdef.png/../../boop.db",
		"2026/09/0123456789abcdef0123456789abcdef.svg", "2026/09/0123456789abcdef0123456789abcdef.png/x",
		"", "2026/09", "2026/09/", string([]byte{0x00}),
	} {
		if file, _, err := Open(opts, key); err == nil {
			file.Close()
			t.Errorf("Open(%q) succeeded, want a not-found error", key)
		} else if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Open(%q) error = %v, want fs.ErrNotExist", key, err)
		}
	}

	if _, _, err := Open(opts, "2026/09/ffffffffffffffffffffffffffffffff.png"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file error = %v, want fs.ErrNotExist", err)
	}
}

func TestContentTypeFollowsTheStoredExtension(t *testing.T) {
	cases := map[string]string{
		"2026/09/0123456789abcdef0123456789abcdef.jpg":  "image/jpeg",
		"2026/09/0123456789abcdef0123456789abcdef.png":  "image/png",
		"2026/09/0123456789abcdef0123456789abcdef.webp": "image/webp",
		"2026/09/0123456789abcdef0123456789abcdef.gif":  "image/gif",
	}
	for key, want := range cases {
		if got := ContentType(key); got != want {
			t.Errorf("ContentType(%q) = %q, want %q", key, got, want)
		}
	}
	if got := ContentType("2026/09/0123456789abcdef0123456789abcdef.svg"); got != "" {
		t.Errorf("ContentType(svg) = %q, want empty", got)
	}
}

func countAssets(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM assets`).Scan(&count); err != nil {
		t.Fatalf("count assets: %v", err)
	}
	return count
}

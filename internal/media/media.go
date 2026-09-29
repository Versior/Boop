// Package media stores owner uploads on the local filesystem: it decides the
// real type of a file from its bytes, keeps client filenames out of every path,
// deduplicates identical content and opens stored files for public serving.
package media

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	// Registering the decoders lets image.DecodeConfig read the header of the
	// three formats the standard library understands; WebP is parsed below
	// because neither Go nor the plan's dependency set ships a WebP decoder.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

const (
	// UploadsDirName is the directory created under BOOP_DATA_DIR.
	UploadsDirName = "uploads"

	// UploadsPath is the public path prefix of a locally stored asset and the
	// route the server registers to serve one. Options.URL is the only place
	// that combines it with a key, so the route and every published address
	// cannot drift apart.
	UploadsPath = "/uploads/"

	// randomKeyBytes makes the public name of a file unguessable: the storage
	// key is the only handle a client ever sees.
	randomKeyBytes = 16

	// sniffBytes is how much of a file http.DetectContentType inspects.
	sniffBytes = 512

	// MaxNameRunes bounds the stored client filename, which is metadata only.
	MaxNameRunes = 200

	// timestampFormat matches the UTC RFC3339 convention of docs/DATABASE.md.
	timestampFormat = time.RFC3339Nano
)

// Errors the HTTP layer maps onto documented responses.
var (
	ErrUnsupportedMedia = errors.New("media: content is not a supported image")
	ErrInvalidFilename  = errors.New("media: filename extension does not match the content")
	ErrTooLarge         = errors.New("media: file exceeds the upload limit")
)

// imageExtensions is the canonical extension per accepted MIME type.
var imageExtensions = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
	"image/gif":  ".gif",
}

// extensionMIME accepts .jpeg as an alias of .jpg.
var extensionMIME = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
	".gif":  "image/gif",
}

// IsImageMIME reports whether a stored MIME type is one this package produces.
func IsImageMIME(mime string) bool {
	_, ok := imageExtensions[mime]
	return ok
}

// Options is the storage location and the per-file ceiling. Object is the zero
// value in the default deployment, where uploads are files under DataDir.
type Options struct {
	DataDir  string
	MaxBytes int64
	Object   ObjectOptions
}

// Asset is a stored upload.
type Asset struct {
	ID           int64
	StorageKey   string
	OriginalName string
	MimeType     string
	SizeBytes    int64
	Width        *int
	Height       *int
	SHA256       [sha256.Size]byte
	CreatedAt    string
	// Reused reports that identical bytes owned by the same user were already
	// stored, so no new row or file was created.
	Reused bool
}

// assetRow is the row Store intends to write.
type assetRow struct {
	OwnerID      int64
	StorageKey   string
	OriginalName string
	MimeType     string
	SizeBytes    int64
	Width        *int
	Height       *int
	SHA256       [sha256.Size]byte
	CreatedAt    string
}

// Root is the absolute upload directory under BOOP_DATA_DIR.
func (o Options) Root() string {
	return filepath.Join(o.DataDir, UploadsDirName)
}

// URL is the address a client uses to read a stored asset. It is the single
// place that turns a storage key into a public URL: the JSON payloads, the feed
// cards and the link-preview image all come through here, so every copy of an
// asset's address is built from the same rule.
//
// When the bytes live in a bucket the address is the bucket's public hostname
// rather than this process, so a reader's browser fetches an image from the CDN
// and the 1-core server never carries the bytes.
func (o Options) URL(key string) string {
	if o.Object.PublicURL == "" {
		return UploadsPath + key
	}
	return strings.TrimSuffix(o.Object.PublicURL, "/") + "/" + encodePath(o.Object.ObjectKey(key))
}

// Remote reports whether stored assets are read from somewhere other than this
// process, which is what the asset route needs in order to answer with a
// redirect instead of opening a file.
func (o Options) Remote() bool {
	return o.Object.PublicURL != ""
}

// Store persists data as an asset of ownerID and returns it. Identical bytes
// already owned by the same user are reused instead of stored twice, and a
// failure after the file was written removes that file again.
func Store(ctx context.Context, db *sql.DB, opts Options, ownerID int64, filename string, data []byte, now time.Time) (*Asset, error) {
	if db == nil {
		return nil, errors.New("media: store: nil database")
	}
	if ownerID <= 0 {
		return nil, errors.New("media: store: owner id is required")
	}
	if opts.MaxBytes > 0 && int64(len(data)) > opts.MaxBytes {
		return nil, ErrTooLarge
	}
	mime, ext, err := sniff(filename, data)
	if err != nil {
		return nil, err
	}

	digest := sha256.Sum256(data)
	// Cheap path for a repeated upload: no file and no transaction.
	if existing, err := findByHash(ctx, db, ownerID, digest); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}

	key, err := newStorageKey(now, ext)
	if err != nil {
		return nil, err
	}
	if err := storeFile(ctx, opts, key, data); err != nil {
		return nil, err
	}

	width, height, hasDimensions := dimensions(mime, data)
	row := assetRow{
		OwnerID:      ownerID,
		StorageKey:   key,
		OriginalName: safeOriginalName(filename),
		MimeType:     mime,
		SizeBytes:    int64(len(data)),
		SHA256:       digest,
		CreatedAt:    now.UTC().Format(timestampFormat),
	}
	if hasDimensions {
		row.Width, row.Height = &width, &height
	}

	asset, reused, err := insertOrReuse(ctx, db, row)
	if err != nil {
		// An asset row no file exists for is fine; a file no row points at is not.
		removeFile(ctx, opts, key)
		return nil, err
	}
	if reused {
		// A concurrent upload of the same bytes won the race: keep its row and
		// file, drop ours.
		removeFile(ctx, opts, key)
	}
	return asset, nil
}

// insertOrReuse writes the row inside one transaction and re-checks the content
// hash inside it, so a check-then-insert race cannot create a duplicate row. It
// reports true and returns the winning row when another writer stored the same
// bytes first.
func insertOrReuse(ctx context.Context, db *sql.DB, row assetRow) (*Asset, bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("media: begin: %w", err)
	}
	defer tx.Rollback()

	if existing, err := findByHash(ctx, tx, row.OwnerID, row.SHA256); err != nil {
		return nil, false, err
	} else if existing != nil {
		return existing, true, nil
	}

	result, err := tx.ExecContext(ctx,
		`INSERT INTO assets(owner_user_id, storage_key, original_name, mime_type, size_bytes, width, height, sha256, created_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		row.OwnerID, row.StorageKey, row.OriginalName, row.MimeType, row.SizeBytes,
		nullInt(row.Width), nullInt(row.Height), row.SHA256[:], row.CreatedAt)
	if err != nil {
		return nil, false, fmt.Errorf("media: insert asset: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, false, fmt.Errorf("media: asset id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("media: commit: %w", err)
	}

	return &Asset{
		ID: id, StorageKey: row.StorageKey, OriginalName: row.OriginalName, MimeType: row.MimeType,
		SizeBytes: row.SizeBytes, Width: row.Width, Height: row.Height,
		SHA256: row.SHA256, CreatedAt: row.CreatedAt,
	}, false, nil
}

// Open resolves a public storage key to a file inside the upload root. Keys
// that are not exactly the generated shape are reported as missing, which is
// what makes directory traversal unreachable.
func Open(opts Options, key string) (*os.File, os.FileInfo, error) {
	if !ValidStorageKey(key) {
		return nil, nil, fs.ErrNotExist
	}
	root := opts.Root()
	full := filepath.Join(root, filepath.FromSlash(key))
	if !within(root, full) {
		return nil, nil, fs.ErrNotExist
	}
	file, err := os.Open(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, fs.ErrNotExist
		}
		return nil, nil, fmt.Errorf("media: open %s: %w", key, err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, fmt.Errorf("media: stat %s: %w", key, err)
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, nil, fs.ErrNotExist
	}
	return file, info, nil
}

// ContentType maps a storage key extension to the header value to serve.
func ContentType(key string) string {
	return extensionMIME[strings.ToLower(path.Ext(key))]
}

// ValidStorageKey reports whether key is exactly <YYYY>/<MM>/<32 hex>.<ext>.
func ValidStorageKey(key string) bool {
	parts := strings.Split(key, "/")
	if len(parts) != 3 || len(parts[0]) != 4 || len(parts[1]) != 2 {
		return false
	}
	if !digitsOnly(parts[0]) || !digitsOnly(parts[1]) {
		return false
	}
	name := parts[2]
	if strings.IndexByte(name, '.') != 32 {
		return false
	}
	if !lowerHex(parts[2][:32]) {
		return false
	}
	return ContentType(name) != ""
}

func digitsOnly(value string) bool {
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func lowerHex(value string) bool {
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// sniff resolves the real type of the bytes and requires the client extension
// to agree with it, which is what rejects an extension that lies about the
// content.
func sniff(filename string, data []byte) (mime, ext string, err error) {
	if len(data) == 0 {
		return "", "", ErrUnsupportedMedia
	}
	head := data
	if len(head) > sniffBytes {
		head = head[:sniffBytes]
	}
	mime = canonicalMIME(http.DetectContentType(head))
	if !IsImageMIME(mime) {
		return "", "", ErrUnsupportedMedia
	}
	declared := strings.ToLower(path.Ext(strings.TrimSpace(filename)))
	if want, ok := extensionMIME[declared]; !ok || want != mime {
		return "", "", ErrInvalidFilename
	}
	return mime, imageExtensions[mime], nil
}

// canonicalMIME folds the aliases DetectContentType can return onto the MIME
// types this package stores.
func canonicalMIME(detected string) string {
	switch detected {
	case "image/jpeg", "image/jpg":
		return "image/jpeg"
	case "image/png":
		return "image/png"
	case "image/gif":
		return "image/gif"
	case "image/webp":
		return "image/webp"
	default:
		return detected
	}
}

// safeOriginalName keeps only the base name of a client filename: it is stored
// as metadata and must never influence a path.
func safeOriginalName(filename string) string {
	name := strings.ReplaceAll(strings.TrimSpace(filename), `\`, "/")
	name = strings.TrimSpace(path.Base(name))
	name = strings.Trim(name, ".")
	if name == "" {
		return "upload"
	}
	if utf8.RuneCountInString(name) > MaxNameRunes {
		name = string([]rune(name)[:MaxNameRunes])
	}
	return name
}

// newStorageKey builds the public name: a random component plus the extension
// that matches the sniffed type.
func newStorageKey(now time.Time, ext string) (string, error) {
	var buf [randomKeyBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("media: random key: %w", err)
	}
	return now.UTC().Format("2006/01") + "/" + hex.EncodeToString(buf[:]) + ext, nil
}

// storeFile writes the bytes of one asset to whichever backend the
// configuration names. The two paths share one contract: on success the key
// resolves to the bytes, and on failure nothing is left behind under that key.
func storeFile(ctx context.Context, opts Options, key string, data []byte) error {
	if opts.Object.Enabled() {
		return opts.Object.put(ctx, key, data)
	}
	return writeFile(opts, key, data)
}

// writeFile writes through a temporary file in the target directory and renames
// it, so a partially written upload is never visible under its key.
func writeFile(opts Options, key string, data []byte) error {
	full := filepath.Join(opts.Root(), filepath.FromSlash(key))
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("media: create upload directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return fmt.Errorf("media: create temp file: %w", err)
	}
	name := temp.Name()
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		os.Remove(name)
		return fmt.Errorf("media: write upload: %w", err)
	}
	if err := temp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("media: close upload: %w", err)
	}
	if err := os.Rename(name, full); err != nil {
		os.Remove(name)
		return fmt.Errorf("media: publish upload: %w", err)
	}
	return nil
}

// removeFile undoes a store. It is best effort in both backends, and for the
// same reason: the only caller is a store that is already failing, and the
// asset it leaves behind is addressed by a random name nothing points at.
func removeFile(ctx context.Context, opts Options, key string) {
	if opts.Object.Enabled() {
		opts.Object.delete(ctx, key)
		return
	}
	os.Remove(filepath.Join(opts.Root(), filepath.FromSlash(key)))
}

// within reports whether full stays inside root.
func within(root, full string) bool {
	relative, err := filepath.Rel(root, full)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

// rowQuerier is satisfied by *sql.DB and *sql.Tx, so the content-hash lookup
// works both before and inside the storing transaction.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func findByHash(ctx context.Context, q rowQuerier, ownerID int64, digest [sha256.Size]byte) (*Asset, error) {
	var asset Asset
	var width, height sql.NullInt64
	var storedHash []byte
	err := q.QueryRowContext(ctx,
		`SELECT id, storage_key, original_name, mime_type, size_bytes, width, height, sha256, created_at
		 FROM assets WHERE owner_user_id = ? AND sha256 = ? ORDER BY id LIMIT 1`,
		ownerID, digest[:],
	).Scan(&asset.ID, &asset.StorageKey, &asset.OriginalName, &asset.MimeType, &asset.SizeBytes,
		&width, &height, &storedHash, &asset.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("media: find asset by content: %w", err)
	}
	copy(asset.SHA256[:], storedHash)
	if width.Valid {
		value := int(width.Int64)
		asset.Width = &value
	}
	if height.Valid {
		value := int(height.Int64)
		asset.Height = &value
	}
	asset.Reused = true
	return &asset, nil
}

func nullInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

// dimensions reads the pixel size from the image header. Unknown or damaged
// headers are reported as absent instead of failing the upload: the columns are
// nullable by design.
func dimensions(mime string, data []byte) (width, height int, ok bool) {
	if mime == "image/webp" {
		return webpDimensions(data)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return 0, 0, false
	}
	return config.Width, config.Height, true
}

// webpDimensions covers the three WebP container variants: VP8X (extended
// canvas), VP8L (lossless bitstream) and VP8 (lossy bitstream).
func webpDimensions(data []byte) (int, int, bool) {
	if len(data) < 16 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, 0, false
	}
	switch string(data[12:16]) {
	case "VP8X":
		if len(data) < 30 {
			return 0, 0, false
		}
		// 4 bytes flags/reserved, then canvas width-1 and height-1 as 24-bit LE.
		width := int(data[24]) | int(data[25])<<8 | int(data[26])<<16
		height := int(data[27]) | int(data[28])<<8 | int(data[29])<<16
		return width + 1, height + 1, true
	case "VP8L":
		if len(data) < 25 || data[20] != 0x2f {
			return 0, 0, false
		}
		bits := binary.LittleEndian.Uint32(data[21:25])
		return int(bits&0x3fff) + 1, int((bits>>14)&0x3fff) + 1, true
	case "VP8 ":
		if len(data) < 30 {
			return 0, 0, false
		}
		if data[23] != 0x9d || data[24] != 0x01 || data[25] != 0x2a {
			return 0, 0, false
		}
		width := int(binary.LittleEndian.Uint16(data[26:28]) & 0x3fff)
		height := int(binary.LittleEndian.Uint16(data[28:30]) & 0x3fff)
		return width, height, true
	default:
		return 0, 0, false
	}
}

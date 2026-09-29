package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Object storage: the same uploads, kept in an S3-compatible bucket instead of
// under BOOP_DATA_DIR. Cloudflare R2 is the target — it is S3-compatible, it
// charges nothing for egress, and a 10GB free tier covers a personal blog — but
// nothing here is R2-specific: an endpoint, a bucket and a key pair are the
// whole contract, so MinIO and S3 work too.
//
// Two operations are implemented, not the nine an SDK offers. Boop writes an
// object once under a name nobody can guess and never rewrites it, so it needs
// PutObject to store one and DeleteObject to undo a store that failed. Reading
// is the bucket's own public hostname, which is what the browser is sent to:
// proxying images through a 1-core process to save a redirect would spend the
// memory budget on work the CDN does better.

const (
	// objectTimeout bounds one object operation. An upload is at most
	// BOOP_MAX_UPLOAD_MB, so this is generous for the bytes and strict enough
	// that an unreachable bucket cannot pin a request goroutine.
	objectTimeout = 30 * time.Second

	// errorBodyBytes is how much of a failed response is quoted back. An S3
	// error document is a few lines of XML; the cap keeps a misconfigured
	// endpoint that returns a whole HTML page from filling a log line.
	errorBodyBytes = 2048
)

// ErrObjectStorage is returned when the bucket refuses an operation: bad
// credentials, a missing bucket, a clock too far from the service. It never
// carries the secret.
var ErrObjectStorage = errors.New("media: object storage rejected the request")

// objectClient is shared by the whole process. It owns the connection pool, so
// consecutive uploads reuse one TLS connection rather than paying a handshake
// each time.
var objectClient = &http.Client{
	Timeout: objectTimeout,
	// A redirect from a bucket endpoint means the configuration is wrong. Follow
	// one and the signature, which is bound to one host, no longer matches.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// ObjectOptions configures an S3-compatible bucket. An empty Endpoint leaves
// uploads under BOOP_DATA_DIR; anything else switches them to the bucket, and
// then every other field is required.
type ObjectOptions struct {
	// Endpoint is the S3 API address, for R2
	// https://<account-id>.r2.cloudflarestorage.com.
	Endpoint string
	// Region is the signing region. R2 signs with "auto", which is the default.
	Region string
	// Bucket is the bucket name.
	Bucket string
	// Prefix is an optional key prefix inside the bucket, without slashes.
	Prefix string
	// PublicURL is the hostname the object is read from: an R2 custom domain or
	// the r2.dev address of the bucket. Reading goes through it, never through
	// the signed API endpoint, which is not a public read surface.
	PublicURL string
	// AccessKey and SecretKey are the R2 API token's key pair.
	AccessKey string
	SecretKey string
}

// Enabled reports whether uploads go to a bucket. Any part of the configuration
// being present means the operator meant to use one: a half-filled
// configuration is a mistake to report, not something to quietly ignore.
func (o ObjectOptions) Enabled() bool {
	return o.Endpoint != "" || o.Bucket != "" || o.Prefix != "" || o.PublicURL != ""
}

// RegionOrDefault is the signing region, defaulting to what R2 expects.
func (o ObjectOptions) RegionOrDefault() string {
	if o.Region == "" {
		return "auto"
	}
	return o.Region
}

// Validate checks the shape of an enabled object configuration: the two
// addresses that get parsed and signed. Whether anything is missing is the
// caller's question, because only the caller knows what the fields are called
// on the surface an operator sees. The prefix is not checked here because
// config normalises it to its bare form before it ever gets this far.
func (o ObjectOptions) Validate() error {
	if !o.Enabled() {
		return nil
	}
	if o.Endpoint != "" {
		if _, err := objectBase(o.Endpoint); err != nil {
			return fmt.Errorf("endpoint: %w", err)
		}
	}
	if o.PublicURL != "" {
		if _, err := objectBase(o.PublicURL); err != nil {
			return fmt.Errorf("public URL: %w", err)
		}
	}
	return nil
}

// ObjectKey is the key an asset is stored under in the bucket: the prefix, then
// the storage key. It is what the signed request addresses.
func (o ObjectOptions) ObjectKey(key string) string {
	if o.Prefix == "" {
		return key
	}
	return o.Prefix + "/" + key
}

// put writes one object. The request declares the payload hash, signs the exact
// headers it sends, and lets the bucket reject anything it disagrees with.
func (o ObjectOptions) put(ctx context.Context, key string, payload []byte) error {
	req, err := o.newRequest(ctx, http.MethodPut, key, payload)
	if err != nil {
		return err
	}
	// An object is written once under a name nobody can guess, so it is as
	// immutable as the local file it replaces, and a browser should treat it the
	// same way whichever copy it came from.
	req.Header.Set("Content-Type", ContentType(key))
	req.Header.Set("Cache-Control", "public, max-age=31536000, immutable")

	return o.do(req, "put", key)
}

// delete removes one object. media.Store calls it to undo a write whose row
// could not be committed, and again when a concurrent upload of identical bytes
// won the race, so an orphaned object never outlives the row that explained it.
func (o ObjectOptions) delete(ctx context.Context, key string) error {
	req, err := o.newRequest(ctx, http.MethodDelete, key, nil)
	if err != nil {
		return err
	}
	return o.do(req, "delete", key)
}

// newRequest builds and signs one request for an object. The signed path and
// the path on the wire are the same bytes by construction: Path carries the
// decoded form and RawPath the encoding, so url.URL.EscapedPath returns exactly
// what canonicalURI signed.
func (o ObjectOptions) newRequest(ctx context.Context, method, key string, payload []byte) (*http.Request, error) {
	if !ValidStorageKey(key) {
		return nil, fs.ErrNotExist
	}
	base, err := objectBase(o.Endpoint)
	if err != nil {
		return nil, err
	}

	decodedPath := strings.TrimSuffix(base.Path, "/") + "/" + o.Bucket + "/" + o.ObjectKey(key)
	target := *base
	target.Path = decodedPath
	target.RawPath = encodePath(decodedPath)

	req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("media: build object request: %w", err)
	}
	// The decoded and encoded forms have to survive url.URL.String, which
	// prefers RawPath and drops it when it does not round-trip.
	req.URL.Path = decodedPath
	req.URL.RawPath = target.RawPath

	payloadHash := hexSHA256(payload)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	signRequest(req, sigv4Credentials{
		AccessKey: o.AccessKey,
		SecretKey: o.SecretKey,
		Region:    o.RegionOrDefault(),
		Service:   "s3",
	}, payloadHash, time.Now())
	return req, nil
}

// do sends a signed request and turns a refusal into an error that names the
// operation without echoing the Authorization header.
func (o ObjectOptions) do(req *http.Request, operation, key string) error {
	response, err := objectClient.Do(req)
	if err != nil {
		return fmt.Errorf("media: object %s %s: %w", operation, key, err)
	}
	defer response.Body.Close()

	if response.StatusCode >= 200 && response.StatusCode < 300 {
		// Nothing to read: both operations answer without a body, and draining
		// one keeps the connection reusable for the next upload.
		io.Copy(io.Discard, io.LimitReader(response.Body, errorBodyBytes))
		return nil
	}
	detail := objectErrorDetail(response)

	switch response.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("media: object %s %s: %w", operation, key, fs.ErrNotExist)
	case http.StatusForbidden, http.StatusUnauthorized:
		return fmt.Errorf("%w: %s %s: %s", ErrObjectStorage, operation, key, detail)
	default:
		return fmt.Errorf("media: object %s %s: %s: %s", operation, key, response.Status, detail)
	}
}

// objectErrorDetail is the bucket's own explanation, trimmed to one line so a
// log entry stays readable. It is quote-safe: the caller formats it with %s into
// a Go error, never into a log line by itself.
func objectErrorDetail(response *http.Response) string {
	body, err := io.ReadAll(io.LimitReader(response.Body, errorBodyBytes))
	if err != nil {
		return "response body unreadable"
	}
	detail := strings.Join(strings.Fields(string(body)), " ")
	if len(detail) > 200 {
		detail = detail[:200] + "…"
	}
	if detail == "" {
		detail = "no detail"
	}
	return detail
}

// objectBase parses the endpoint or public hostname. Credentials, a query and a
// fragment are refused: they would end up in the signed path or silently drop
// out of it.
func objectBase(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%q is not a URL: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("%q must use http or https", raw)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("%q has no host", raw)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("%q must not contain credentials", raw)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("%q must not contain a query or fragment", raw)
	}
	return parsed, nil
}

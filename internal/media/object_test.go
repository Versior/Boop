package media

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBucket stands in for an S3 endpoint. It records what it received so a
// test can inspect the request Boop would have sent to R2, and it keeps the
// objects it was given so a test can prove one was removed again.
type fakeBucket struct {
	mu       sync.Mutex
	url      string
	requests []recordedObjectRequest
	objects  map[string][]byte
	status   int
	body     string
}

type recordedObjectRequest struct {
	method  string
	host    string
	uri     string
	headers http.Header
	body    []byte
}

func newFakeBucket(t *testing.T) *fakeBucket {
	t.Helper()
	bucket := &fakeBucket{
		objects: make(map[string][]byte),
		status:  http.StatusOK,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read object request body: %v", err)
		}

		bucket.mu.Lock()
		bucket.requests = append(bucket.requests, recordedObjectRequest{
			method:  r.Method,
			host:    r.Host,
			uri:     r.RequestURI,
			headers: r.Header.Clone(),
			body:    body,
		})
		status, message := bucket.status, bucket.body
		if status >= 200 && status < 300 {
			if r.Method == http.MethodDelete {
				delete(bucket.objects, r.URL.Path)
			} else {
				bucket.objects[r.URL.Path] = body
			}
		}
		bucket.mu.Unlock()

		w.WriteHeader(status)
		io.WriteString(w, message)
	}))
	t.Cleanup(server.Close)
	bucket.url = server.URL
	return bucket
}

// last is the request under inspection: every test below sends one operation at
// a time, so the most recent request is the one it means.
func (b *fakeBucket) last(t *testing.T) recordedObjectRequest {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.requests) == 0 {
		t.Fatal("no request reached the bucket")
	}
	return b.requests[len(b.requests)-1]
}

func (b *fakeBucket) recorded() []recordedObjectRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]recordedObjectRequest(nil), b.requests...)
}

func (b *fakeBucket) objectCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.objects)
}

// respond makes every following request fail with a status and an error
// document, which is how a bucket refuses bad credentials.
func (b *fakeBucket) respond(status int, body string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.status, b.body = status, body
}

// objectOptions points uploads at a fake bucket instead of a directory.
func objectOptions(t *testing.T, endpoint string) Options {
	t.Helper()
	return Options{
		DataDir:  t.TempDir(),
		MaxBytes: 4 << 20,
		Object: ObjectOptions{
			Endpoint:  endpoint,
			Region:    "auto",
			Bucket:    "boop-uploads",
			Prefix:    "uploads",
			PublicURL: "https://uploads.example.com",
			AccessKey: "an-access-key",
			SecretKey: "a-secret-key",
		},
	}
}

func testObjectKey() string {
	return "2026/09/" + strings.Repeat("ab", 16) + ".png"
}

// verifySignature rebuilds the signature from the request exactly as it arrived
// and insists the two match.
//
// This does not re-derive SigV4 — sigv4_test.go checks that against AWS's own
// vectors. What it checks is the wiring: that the path, the host, the payload
// hash, the timestamp and every signed header were signed with the values that
// went on the wire. Sign one value and send another and the signature changes,
// which is what this catches.
func verifySignature(t *testing.T, options Options, received recordedObjectRequest) {
	t.Helper()
	signed := signedHeaderNames(t, received.headers.Get("Authorization"))
	timestampHeader := received.headers.Get("X-Amz-Date")
	timestamp, err := time.Parse(amzTimestampFormat, timestampHeader)
	if err != nil {
		t.Fatalf("X-Amz-Date %q: %v", timestampHeader, err)
	}
	parsed, err := url.Parse("http://" + received.host + received.uri)
	if err != nil {
		t.Fatalf("parse received URI %q: %v", received.uri, err)
	}

	rebuilt := &http.Request{
		Method: received.method,
		URL:    &url.URL{Scheme: "http", Host: received.host, Path: parsed.Path, RawPath: parsed.RawPath},
		Header: http.Header{},
	}
	for _, name := range signed {
		if name == "host" {
			continue
		}
		rebuilt.Header.Set(name, received.headers.Get(name))
	}

	signRequest(rebuilt, sigv4Credentials{
		AccessKey: options.Object.AccessKey,
		SecretKey: options.Object.SecretKey,
		Region:    options.Object.RegionOrDefault(),
		Service:   "s3",
	}, received.headers.Get("X-Amz-Content-Sha256"), timestamp)

	if got, want := rebuilt.Header.Get("Authorization"), received.headers.Get("Authorization"); got != want {
		t.Errorf("the signature does not describe the request that was sent:\n  sent   %s\n  signed %s", want, got)
	}
}

// signedHeaderNames reads the SignedHeaders list out of an Authorization header
// and insists it is sorted: SigV4 requires the order, so an unsorted list is
// rejected by the service rather than by this test.
func signedHeaderNames(t *testing.T, authorization string) []string {
	t.Helper()
	const marker = "SignedHeaders="
	index := strings.Index(authorization, marker)
	if index < 0 {
		t.Fatalf("Authorization %q has no SignedHeaders", authorization)
	}
	rest := authorization[index+len(marker):]
	if end := strings.IndexByte(rest, ','); end >= 0 {
		rest = rest[:end]
	}
	names := strings.Split(rest, ";")
	if len(names) == 0 || names[0] == "" {
		t.Fatalf("Authorization %q has an empty SignedHeaders list", authorization)
	}
	for index := 1; index < len(names); index++ {
		if names[index-1] >= names[index] {
			t.Errorf("SignedHeaders %q is not sorted", rest)
		}
	}
	return names
}

func TestPutSignsTheRequestItSends(t *testing.T) {
	bucket := newFakeBucket(t)
	options := objectOptions(t, bucket.url)
	data := pngBytes(t, 6, 4)
	key := testObjectKey()

	if err := options.Object.put(context.Background(), key, data); err != nil {
		t.Fatalf("put: %v", err)
	}

	received := bucket.last(t)
	if received.method != http.MethodPut {
		t.Errorf("method = %q, want PUT", received.method)
	}
	// The bucket name and the prefix are part of the path, and the path is what
	// the signature covers.
	if want := "/boop-uploads/uploads/" + key; received.uri != want {
		t.Errorf("request URI = %q, want %q", received.uri, want)
	}
	if got := received.headers.Get("X-Amz-Content-Sha256"); got != hexSHA256(data) {
		t.Errorf("X-Amz-Content-Sha256 = %q, want the hash of the body", got)
	}
	if got := received.headers.Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", got)
	}
	if got := received.headers.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q, want a long immutable cache", got)
	}
	if got := received.headers.Get("Authorization"); !strings.HasPrefix(got, "AWS4-HMAC-SHA256 Credential=an-access-key/") {
		t.Errorf("Authorization = %q, want it to name the access key", got)
	}
	if string(received.body) != string(data) {
		t.Error("the bucket received different bytes than it was given")
	}
	verifySignature(t, options, received)
}

func TestObjectRequestsNeverCarryTheSecret(t *testing.T) {
	bucket := newFakeBucket(t)
	options := objectOptions(t, bucket.url)

	if err := options.Object.put(context.Background(), testObjectKey(), pngBytes(t, 2, 2)); err != nil {
		t.Fatalf("put: %v", err)
	}

	received := bucket.last(t)
	for name, values := range received.headers {
		for _, value := range values {
			if strings.Contains(value, options.Object.SecretKey) {
				t.Errorf("header %s carries the secret key", name)
			}
		}
	}
	if strings.Contains(received.uri, options.Object.SecretKey) {
		t.Error("the request URI carries the secret key")
	}
	if strings.Contains(received.uri, options.Object.AccessKey) {
		t.Error("the request URI carries the access key")
	}
}

func TestPutRefusesAKeyThatIsNotTheGeneratedShape(t *testing.T) {
	bucket := newFakeBucket(t)
	options := objectOptions(t, bucket.url)

	for _, key := range []string{"", "../boop.db", "/etc/passwd", "2026/09/0123456789abcdef0123456789abcdef.svg"} {
		if err := options.Object.put(context.Background(), key, pngBytes(t, 2, 2)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("put(%q) error = %v, want fs.ErrNotExist", key, err)
		}
	}
	if requests := bucket.recorded(); len(requests) != 0 {
		t.Errorf("%d requests reached the bucket for keys that address nothing", len(requests))
	}
}

func TestObjectFailuresAreReported(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantNotFou bool
		wantDenied bool
		wantDetail string
	}{
		{name: "missing", status: http.StatusNotFound, body: "<Error><Code>NoSuchBucket</Code></Error>", wantNotFou: true},
		{name: "denied", status: http.StatusForbidden, body: "<Error><Code>InvalidAccessKeyId</Code></Error>", wantDenied: true},
		{name: "server error", status: http.StatusInternalServerError, body: "<Error><Code>InternalError</Code></Error>", wantDetail: "InternalError"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			bucket := newFakeBucket(t)
			bucket.respond(testCase.status, testCase.body)
			options := objectOptions(t, bucket.url)

			err := options.Object.put(context.Background(), testObjectKey(), pngBytes(t, 2, 2))
			if err == nil {
				t.Fatal("put succeeded against a refusing bucket")
			}
			if testCase.wantNotFou && !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("error = %v, want fs.ErrNotExist", err)
			}
			if testCase.wantDenied && !errors.Is(err, ErrObjectStorage) {
				t.Errorf("error = %v, want ErrObjectStorage", err)
			}
			if testCase.wantDetail != "" && !strings.Contains(err.Error(), testCase.wantDetail) {
				t.Errorf("error = %v, want it to quote the bucket's %s", err, testCase.wantDetail)
			}
			if strings.Contains(err.Error(), options.Object.SecretKey) {
				t.Errorf("error %v carries the secret key", err)
			}
		})
	}
}

func TestStoreKeepsUploadsInTheBucket(t *testing.T) {
	bucket := newFakeBucket(t)
	options := objectOptions(t, bucket.url)
	db := testDB(t)
	owner := insertUser(t, db, "owner@example.com")

	asset, err := Store(context.Background(), db, options, owner, "雾海.png", pngBytes(t, 8, 5), testNow)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	received := bucket.last(t)
	if want := "/boop-uploads/uploads/" + asset.StorageKey; received.uri != want {
		t.Errorf("the object was stored at %q, want %q", received.uri, want)
	}
	if count := bucket.objectCount(); count != 1 {
		t.Errorf("bucket holds %d objects, want 1", count)
	}
	// Nothing may be written under the data directory once the bytes live in a
	// bucket: two copies that can disagree is worse than either one alone.
	if files := filesUnder(t, options.DataDir); len(files) != 0 {
		t.Errorf("local files were written alongside the object: %v", files)
	}
	if got, want := options.URL(asset.StorageKey), "https://uploads.example.com/uploads/"+asset.StorageKey; got != want {
		t.Errorf("URL(%q) = %q, want %q", asset.StorageKey, got, want)
	}
	if !options.Remote() {
		t.Error("Remote() = false, want true when uploads live in a bucket")
	}
}

func TestStoreRemovesTheObjectWhenTheInsertFails(t *testing.T) {
	bucket := newFakeBucket(t)
	options := objectOptions(t, bucket.url)
	db := testDB(t)
	owner := insertUser(t, db, "owner@example.com")

	if _, err := db.Exec(`CREATE TRIGGER assets_block BEFORE INSERT ON assets BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	if _, err := Store(context.Background(), db, options, owner, "a.png", pngBytes(t, 4, 4), testNow); err == nil {
		t.Fatal("Store succeeded although the insert was blocked")
	}

	requests := bucket.recorded()
	if len(requests) != 2 {
		t.Fatalf("bucket saw %d requests, want a put and a delete", len(requests))
	}
	if requests[0].method != http.MethodPut || requests[1].method != http.MethodDelete {
		t.Errorf("methods = %q then %q, want PUT then DELETE", requests[0].method, requests[1].method)
	}
	if requests[0].uri != requests[1].uri {
		t.Errorf("undeleted object: put %q but deleted %q", requests[0].uri, requests[1].uri)
	}
	if count := bucket.objectCount(); count != 0 {
		t.Errorf("bucket still holds %d objects after a failed insert", count)
	}
}

func TestObjectURLStaysLocalWithoutAPublicHostname(t *testing.T) {
	// A public hostname is what makes the bucket readable, so a configuration
	// without one keeps every address on this process. Config refuses that
	// combination at startup; this is the media layer agreeing with it.
	options := Options{DataDir: t.TempDir()}
	key := testObjectKey()
	if got, want := options.URL(key), UploadsPath+key; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	if options.Remote() {
		t.Error("Remote() = true without a public hostname")
	}
}

func TestObjectKeyCarriesThePrefix(t *testing.T) {
	key := testObjectKey()
	cases := map[string]string{
		"":        key,
		"uploads": "uploads/" + key,
	}
	for prefix, want := range cases {
		object := ObjectOptions{Prefix: prefix}
		if got := object.ObjectKey(key); got != want {
			t.Errorf("ObjectKey with prefix %q = %q, want %q", prefix, got, want)
		}
	}
}

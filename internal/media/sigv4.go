package media

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// AWS Signature Version 4, implemented against the specification rather than
// pulled in as a dependency. The object store needs three operations on one
// bucket, and the AWS SDK would add a dozen modules and a plugin registration
// chain for them; the whole algorithm is the sixty lines below.
//
// The constants are the ones the published AWS test vectors use, so
// sigv4_test.go can check this implementation against AWS's own expected
// signatures rather than against itself.
const (
	signatureAlgorithm = "AWS4-HMAC-SHA256"

	// amzTimestampFormat is the ISO 8601 basic timestamp the X-Amz-Date header
	// carries and the string to sign repeats.
	amzTimestampFormat = "20060102T150405Z"

	// amzDateFormat is the day component of the credential scope.
	amzDateFormat = "20060102"

	// emptyPayloadHash is the SHA-256 of a zero-length body. SigV4 hashes the
	// payload of every request, including the ones that carry none, and this
	// literal is the value the AWS vectors show for exactly that case; the test
	// asserts it equals hexSHA256(nil).
	emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// sigv4Credentials is everything the signature is derived from apart from the
// request itself. Service is a field rather than the constant "s3" so the
// published AWS vectors, which sign a service called "service", can be replayed
// against this code.
type sigv4Credentials struct {
	AccessKey string
	SecretKey string
	Region    string
	Service   string
}

// signRequest signs req in place, adding X-Amz-Date and Authorization.
//
// Every other header must already be final, because SigV4 signs the exact set
// of headers that goes on the wire: a header added after signing makes the
// signature invalid. payloadHash is passed in rather than read back from
// X-Amz-Content-Sha256 so that the header stays optional — the published AWS
// test vectors sign a payload hash they do not send — and objectClient is what
// keeps the two in step by deriving both from one hash.
func signRequest(req *http.Request, creds sigv4Credentials, payloadHash string, now time.Time) {
	now = now.UTC()
	timestamp := now.Format(amzTimestampFormat)
	day := now.Format(amzDateFormat)

	if req.Header.Get("X-Amz-Date") == "" {
		req.Header.Set("X-Amz-Date", timestamp)
	}
	if payloadHash == "" {
		payloadHash = emptyPayloadHash
	}

	names, canonicalHeaders := canonicalHeadersOf(req)
	signedHeaders := strings.Join(names, ";")

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{day, creds.Region, creds.Service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		signatureAlgorithm,
		req.Header.Get("X-Amz-Date"),
		scope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(signingKey(creds, day), stringToSign))

	req.Header.Set("Authorization", signatureAlgorithm+
		" Credential="+creds.AccessKey+"/"+scope+
		", SignedHeaders="+signedHeaders+
		", Signature="+signature)
}

// canonicalHeadersOf returns the sorted lowercase header names and the
// canonical header block. The Host header is taken from the URL because Go
// keeps it out of the header map, and it has to be signed: it is what ties the
// signature to one endpoint.
func canonicalHeadersOf(req *http.Request) ([]string, string) {
	values := make(map[string]string, len(req.Header)+1)
	values["host"] = req.URL.Host
	for name, list := range req.Header {
		values[strings.ToLower(name)] = strings.Join(list, ",")
	}

	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	var block strings.Builder
	for _, name := range names {
		block.WriteString(name)
		block.WriteByte(':')
		block.WriteString(collapseSpaces(values[name]))
		block.WriteByte('\n')
	}
	return names, block.String()
}

// collapseSpaces is the header normalisation SigV4 prescribes: leading and
// trailing whitespace goes, and each run of internal whitespace becomes one
// space.
func collapseSpaces(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// signingKey derives the per-day, per-region, per-service key the signature is
// computed with, so the long-term secret never signs a request directly.
func signingKey(creds sigv4Credentials, day string) []byte {
	date := hmacSHA256([]byte("AWS4"+creds.SecretKey), day)
	region := hmacSHA256(date, creds.Region)
	service := hmacSHA256(region, creds.Service)
	return hmacSHA256(service, "aws4_request")
}

// canonicalURI is the encoded path SigV4 signs. It is derived from the decoded
// path rather than taken from url.URL.EscapedPath, because EscapedPath leaves
// "+" alone — Go treats it as an ordinary path character — while the
// specification requires %2B. objectClient sets RawPath to exactly this
// encoding, so the bytes on the wire and the bytes inside the signature agree.
func canonicalURI(u *url.URL) string {
	if u.Path == "" {
		return "/"
	}
	return encodePath(u.Path)
}

// canonicalQuery is the query string with its keys and values sorted and
// encoded. Every request this package sends is path-scoped and carries no
// query, so it normally returns the empty string; it is written out anyway
// because an unsigned query parameter is a silent authentication failure
// rather than a visible one.
func canonicalQuery(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	query := u.Query()
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		values := query[key]
		sort.Strings(values)
		for _, value := range values {
			pairs = append(pairs, uriEncode(key)+"="+uriEncode(value))
		}
	}
	return strings.Join(pairs, "&")
}

// uriEncode percent-encodes one path segment the way SigV4 expects: the
// unreserved characters stay as they are and everything else becomes uppercase
// percent escapes. The caller keeps the slashes that separate segments.
func uriEncode(value string) string {
	var encoded strings.Builder
	encoded.Grow(len(value))
	for index := 0; index < len(value); index++ {
		char := value[index]
		switch {
		case char >= 'A' && char <= 'Z',
			char >= 'a' && char <= 'z',
			char >= '0' && char <= '9',
			char == '-', char == '.', char == '_', char == '~':
			encoded.WriteByte(char)
		default:
			fmt.Fprintf(&encoded, "%%%02X", char)
		}
	}
	return encoded.String()
}

// encodePath percent-encodes every segment of a slash-separated path and leaves
// the slashes themselves alone.
func encodePath(path string) string {
	segments := strings.Split(path, "/")
	for index, segment := range segments {
		segments[index] = uriEncode(segment)
	}
	return strings.Join(segments, "/")
}

func hexSHA256(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func hmacSHA256(key []byte, message string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(message))
	return mac.Sum(nil)
}

package media

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The three requests below are the canonical AWS Signature Version 4 test
// vectors, copied verbatim from the suite the official SDKs test themselves
// against: the request as it goes on the wire and the Authorization header AWS
// says it must produce. Signing code that agrees with itself proves nothing, so
// these expectations come from outside this package.
const (
	vectorAccessKey = "AKIDEXAMPLE"
	vectorSecretKey = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	vectorRegion    = "us-east-1"
	vectorService   = "service"
	vectorTimestamp = "20150830T123600Z"

	vectorVanillaAuth = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date, " +
		"Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"

	vectorQueryAuth = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date, " +
		"Signature=b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500"

	vectorFormAuth = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=content-type;host;x-amz-date, " +
		"Signature=ff11897932ad3f4e8b18135d722051e5ac45fc38421b1da7b9d196a0fe09473a"

	// vectorFormPayloadHash is the canonical request's payload line for the
	// form vector. The test recomputes it from "Param1=value1" rather than
	// trusting this transcription.
	vectorFormPayloadHash = "9095672bbd1f56dfc5b65f3e153adc8731a4a654192329106275f4c7b24d0b6e"
)

func vectorCredentials() sigv4Credentials {
	return sigv4Credentials{
		AccessKey: vectorAccessKey,
		SecretKey: vectorSecretKey,
		Region:    vectorRegion,
		Service:   vectorService,
	}
}

func vectorNow(t *testing.T) time.Time {
	t.Helper()
	parsed, err := time.Parse(amzTimestampFormat, vectorTimestamp)
	if err != nil {
		t.Fatalf("parse vector timestamp: %v", err)
	}
	return parsed
}

func TestSignRequestReproducesTheAWSTestVectors(t *testing.T) {
	cases := []struct {
		name        string
		method      string
		target      string
		header      map[string]string
		body        string
		payloadHash string
		wantAuth    string
	}{
		{
			name:     "get-vanilla",
			method:   http.MethodGet,
			target:   "http://example.amazonaws.com/",
			wantAuth: vectorVanillaAuth,
		},
		{
			name:   "get-vanilla-query-order-key-case",
			method: http.MethodGet,
			// The vector lists Param2 first on purpose: the canonical query is
			// sorted by key, so the two must still sign to the same value.
			target:   "http://example.amazonaws.com/?Param2=value2&Param1=value1",
			wantAuth: vectorQueryAuth,
		},
		{
			name:   "post-x-www-form-urlencoded",
			method: http.MethodPost,
			target: "http://example.amazonaws.com/",
			header: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
			body:   "Param1=value1",
			// The vector signs the hash of the body, not the empty-body
			// fallback, so this is what a real PUT looks like.
			payloadHash: vectorFormPayloadHash,
			wantAuth:    vectorFormAuth,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			req, err := http.NewRequest(testCase.method, testCase.target, strings.NewReader(testCase.body))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			for name, value := range testCase.header {
				req.Header.Set(name, value)
			}
			req.Header.Set("X-Amz-Date", vectorTimestamp)

			signRequest(req, vectorCredentials(), testCase.payloadHash, vectorNow(t))

			if got := req.Header.Get("Authorization"); got != testCase.wantAuth {
				t.Errorf("Authorization =\n  %s\nwant\n  %s", got, testCase.wantAuth)
			}
		})
	}
}

func TestSignRequestMintsTheTimestampWhenAbsent(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://example.amazonaws.com/", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	signRequest(req, vectorCredentials(), "", vectorNow(t))

	if got := req.Header.Get("X-Amz-Date"); got != vectorTimestamp {
		t.Errorf("X-Amz-Date = %q, want %q", got, vectorTimestamp)
	}
}

// The literal in the source is what the AWS vectors show for a request with no
// body; this is what keeps it honest.
func TestEmptyPayloadHashIsTheHashOfNothing(t *testing.T) {
	if got := hexSHA256(nil); got != emptyPayloadHash {
		t.Errorf("hexSHA256(nil) = %q, want %q", got, emptyPayloadHash)
	}
}

// The form vector's payload line is a published constant; checking it against
// the hash of the body it was published for keeps the transcription in this
// file from drifting away from the suite it came from.
func TestTheFormVectorSignsItsOwnBody(t *testing.T) {
	if got := hexSHA256([]byte("Param1=value1")); got != vectorFormPayloadHash {
		t.Errorf("sha256(Param1=value1) = %q, want the published %q", got, vectorFormPayloadHash)
	}
}

// A request that carries a body signs differently from one that lets the
// empty-body fallback apply, which is what makes the fallback a fallback rather
// than the only path.
func TestSignRequestSignsTheGivenPayloadHash(t *testing.T) {
	signed := func(payloadHash string) string {
		req, err := http.NewRequest(http.MethodPut, "http://example.amazonaws.com/boop/a.png", nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		signRequest(req, vectorCredentials(), payloadHash, vectorNow(t))
		return req.Header.Get("Authorization")
	}

	if signed(hexSHA256([]byte("the bytes"))) == signed("") {
		t.Error("a declared payload hash produced the same signature as the empty-body fallback")
	}
}

func TestCanonicalURIEncodesExactlyOnce(t *testing.T) {
	cases := map[string]string{
		"/":                   "/",
		"/boop/2026/09/a.png": "/boop/2026/09/a.png",
		"":                    "/",
		"/boop/已上传 的 照片.png":  "/boop/%E5%B7%B2%E4%B8%8A%E4%BC%A0%20%E7%9A%84%20%E7%85%A7%E7%89%87.png",
		"/boop/a+b.png":       "/boop/a%2Bb.png",
	}
	for input, want := range cases {
		parsed, err := url.Parse("http://example.amazonaws.com" + input)
		if err != nil {
			t.Fatalf("parse %q: %v", input, err)
		}
		if got := canonicalURI(parsed); got != want {
			t.Errorf("canonicalURI(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCanonicalQuerySortsByKeyAndValue(t *testing.T) {
	cases := map[string]string{
		"":                            "",
		"Param2=value2&Param1=value1": "Param1=value1&Param2=value2",
		"b=2&a=3&a=1":                 "a=1&a=3&b=2",
		"prefix=a/b":                  "prefix=a%2Fb",
		"name=已上传":                    "name=%E5%B7%B2%E4%B8%8A%E4%BC%A0",
	}
	for raw, want := range cases {
		parsed, err := url.Parse("http://example.amazonaws.com/?" + raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		if got := canonicalQuery(parsed); got != want {
			t.Errorf("canonicalQuery(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestUriEncodeKeepsOnlyTheUnreservedCharacters(t *testing.T) {
	const unreserved = "AZaz09-._~"
	if got := uriEncode(unreserved); got != unreserved {
		t.Errorf("uriEncode(%q) = %q, want it unchanged", unreserved, got)
	}
	cases := map[string]string{
		"a b": "a%20b",
		"a/b": "a%2Fb",
		"a+b": "a%2Bb",
		"a*b": "a%2Ab",
		"a%b": "a%25b",
		"已上传": "%E5%B7%B2%E4%B8%8A%E4%BC%A0",
	}
	for input, want := range cases {
		if got := uriEncode(input); got != want {
			t.Errorf("uriEncode(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestEncodePathKeepsTheSeparators(t *testing.T) {
	if got, want := encodePath("/boop/2026/09/a b.png"), "/boop/2026/09/a%20b.png"; got != want {
		t.Errorf("encodePath = %q, want %q", got, want)
	}
}

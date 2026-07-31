package main

import (
	"os"
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"
	"github.com/versity/versitygw/s3err"
)

func TestParseConfig(t *testing.T) {
	got := parseConfig("namespace=examplens, region=us-phoenix-1 auth=workload_identity")
	for k, want := range map[string]string{
		"namespace": "examplens",
		"region":    "us-phoenix-1",
		"auth":      "workload_identity",
	} {
		if got[k] != want {
			t.Errorf("parseConfig()[%q] = %q, want %q", k, got[k], want)
		}
	}
	if len(parseConfig("")) != 0 {
		t.Error("empty config should parse to an empty map")
	}
	// A bare token with no '=' must be skipped, not panic or half-parse.
	if len(parseConfig("garbage")) != 0 {
		t.Error("token without '=' should be ignored")
	}
}

func TestSplitCopySource(t *testing.T) {
	tests := []struct {
		in          string
		bucket, key string
		wantErr     bool
	}{
		{in: "/bucket/some/deep/key.txt", bucket: "bucket", key: "some/deep/key.txt"},
		{in: "bucket/key", bucket: "bucket", key: "key"},
		{in: "/bucket/key?versionId=abc", bucket: "bucket", key: "key"},
		{in: "/onlybucket", wantErr: true},
		{in: "", wantErr: true},
		{in: "/", wantErr: true},
	}
	for _, tc := range tests {
		b, k, err := splitCopySource(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("splitCopySource(%q) = (%q,%q), want error", tc.in, b, k)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitCopySource(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if b != tc.bucket || k != tc.key {
			t.Errorf("splitCopySource(%q) = (%q,%q), want (%q,%q)", tc.in, b, k, tc.bucket, tc.key)
		}
	}
}

func TestQuoteRoundTrip(t *testing.T) {
	// S3 clients expect quoted ETags; OCI returns them bare. Double-quoting
	// breaks If-Match, and committing a quoted ETag back to OCI fails the part.
	if got := quote("abc"); got != `"abc"` {
		t.Errorf("quote(abc) = %s", got)
	}
	if got := quote(`"abc"`); got != `"abc"` {
		t.Errorf("quote should not double-quote, got %s", got)
	}
	if got := quote(""); got != "" {
		t.Errorf("quote(empty) = %q, want empty", got)
	}
	if got := unquote(`"abc"`); got != "abc" {
		t.Errorf("unquote = %s", got)
	}
}

func TestConvertListing(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	size := int64(42)
	etag := "deadbeef"
	name := "a/b.txt"
	resp := objectstorage.ListObjectsResponse{
		ListObjects: objectstorage.ListObjects{
			Objects: []objectstorage.ObjectSummary{
				{Name: &name, Size: &size, Etag: &etag, TimeModified: &common.SDKTime{Time: now}},
			},
			Prefixes: []string{"a/", "c/"},
		},
	}
	contents, prefixes := convertListing(resp)
	if len(contents) != 1 {
		t.Fatalf("got %d contents, want 1", len(contents))
	}
	if *contents[0].Key != name || *contents[0].Size != size {
		t.Errorf("object not mapped: %+v", contents[0])
	}
	if *contents[0].ETag != `"deadbeef"` {
		t.Errorf("ETag = %s, want quoted", *contents[0].ETag)
	}
	if !contents[0].LastModified.Equal(now) {
		t.Errorf("LastModified = %v, want %v", contents[0].LastModified, now)
	}
	if len(prefixes) != 2 || *prefixes[0].Prefix != "a/" || *prefixes[1].Prefix != "c/" {
		t.Errorf("common prefixes not mapped: %+v", prefixes)
	}
	// Aliasing bug guard: taking &resp.Prefixes[i] is required, not &loopVar.
	if prefixes[0].Prefix == prefixes[1].Prefix {
		t.Error("common prefixes alias the same pointer")
	}
}

type fakeSvcErr struct {
	status int
	code   string
}

func (f fakeSvcErr) Error() string           { return f.code }
func (f fakeSvcErr) GetHTTPStatusCode() int  { return f.status }
func (f fakeSvcErr) GetMessage() string      { return f.code }
func (f fakeSvcErr) GetCode() string         { return f.code }
func (f fakeSvcErr) GetOpcRequestID() string { return "" }

func TestMapError(t *testing.T) {
	var _ common.ServiceError = fakeSvcErr{}

	if mapError(nil) != nil {
		t.Error("nil should map to nil")
	}

	// A missing bucket and a missing key are both 404 from OCI; clients must be
	// able to tell them apart or a registry's blob upload path misbehaves.
	bucketErr := mapError(fakeSvcErr{status: 404, code: "BucketNotFound"})
	if bucketErr != s3err.GetAPIError(s3err.ErrNoSuchBucket) {
		t.Errorf("BucketNotFound mapped to %v", bucketErr)
	}
	keyErr := mapError(fakeSvcErr{status: 404, code: "ObjectNotFound"})
	if keyErr != s3err.GetAPIError(s3err.ErrNoSuchKey) {
		t.Errorf("ObjectNotFound mapped to %v", keyErr)
	}
	if denied := mapError(fakeSvcErr{status: 403, code: "NotAuthenticated"}); denied != s3err.GetAPIError(s3err.ErrAccessDenied) {
		t.Errorf("403 mapped to %v", denied)
	}

	// Anything unrecognised must pass through rather than be flattened into a
	// misleading S3 code.
	orig := fakeSvcErr{status: 500, code: "InternalServerError"}
	if got := mapError(orig); got != error(orig) {
		t.Errorf("unmapped error should pass through, got %v", got)
	}
}

func TestIsNotFound(t *testing.T) {
	if !isNotFound(fakeSvcErr{status: 404, code: "ObjectNotFound"}) {
		t.Error("404 should be not-found")
	}
	if isNotFound(fakeSvcErr{status: 500, code: "boom"}) {
		t.Error("500 is not not-found")
	}
}

func TestParseConfigFromFile(t *testing.T) {
	// versitygw passes `--config` as a file path, so a path must be read rather
	// than parsed as if it were the config itself.
	dir := t.TempDir()
	path := dir + "/plugin.conf"
	if err := os.WriteFile(path, []byte("namespace=examplens\nregion=us-phoenix-1\nauth=workload_identity\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := parseConfig(path)
	if got["namespace"] != "examplens" || got["region"] != "us-phoenix-1" || got["auth"] != "workload_identity" {
		t.Errorf("config file not parsed: %+v", got)
	}
}

func TestNilIfEmpty(t *testing.T) {
	// Regression: versitygw hands the backend non-nil empty strings for absent
	// Prefix/Delimiter/Start and for a whole-object Range. OCI rejects those as
	// `param=` with 400 InvalidObjectName ("The delimiter parameter '' is not
	// supported") and 416 InvalidRange, which surface to clients as a 500 on
	// plain ListObjectsV2 and GetObject. Both were caught only in live e2e.
	if nilIfEmpty(nil) != nil {
		t.Error("nil should stay nil")
	}
	empty := ""
	if nilIfEmpty(&empty) != nil {
		t.Error("empty string must be dropped, not sent as param=")
	}
	val := "a/"
	if got := nilIfEmpty(&val); got == nil || *got != "a/" {
		t.Error("non-empty value must pass through")
	}
}

func TestConvertVersions(t *testing.T) {
	// Found only after the first consumer cutover: the obsidian exporter issues
	// GET /bucket?versions for storage accounting and reported scrape_success=0
	// while ListObjectVersions fell through to BackendUnsupported.
	now := time.Now().UTC().Truncate(time.Second)
	tru, fls := true, false
	k1, k2 := "a.txt", "b.txt"
	v1, v2, v3 := "v1", "v2", "vd"
	s1, s2 := int64(10), int64(20)
	items := []objectstorage.ObjectVersionSummary{
		{Name: &k1, VersionId: &v1, Size: &s1, IsDeleteMarker: &fls, TimeModified: &common.SDKTime{Time: now}},
		{Name: &k1, VersionId: &v2, Size: &s2, IsDeleteMarker: &fls, TimeModified: &common.SDKTime{Time: now}},
		{Name: &k2, VersionId: &v3, IsDeleteMarker: &tru, TimeModified: &common.SDKTime{Time: now}},
	}
	versions, markers := convertVersions(items)

	if len(versions) != 2 || len(markers) != 1 {
		t.Fatalf("split wrong: %d versions, %d markers", len(versions), len(markers))
	}
	// Sizes must survive: the exporter sums them, and a nil here silently
	// reports a bucket as 0 bytes rather than failing loudly.
	if *versions[0].Size != 10 || *versions[1].Size != 20 {
		t.Errorf("sizes not mapped: %+v", versions)
	}
	// First sighting of a key is latest; the second version of the same key is not.
	if !*versions[0].IsLatest {
		t.Error("first version of a key should be IsLatest")
	}
	if *versions[1].IsLatest {
		t.Error("second version of the same key must not be IsLatest")
	}
	if markers[0].Key == nil || *markers[0].Key != k2 {
		t.Errorf("delete marker not mapped: %+v", markers[0])
	}
	if vs, ms := convertVersions(nil); len(vs) != 0 || len(ms) != 0 {
		t.Error("nil input should produce no versions or markers")
	}
}

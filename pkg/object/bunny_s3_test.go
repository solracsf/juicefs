//go:build !nos3

/*
 * JuiceFS, Copyright 2026 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package object

import (
	"bytes"
	"cmp"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/juicedata/juicefs/pkg/utils"
	"golang.org/x/sync/errgroup"
)

// fakeBunnyS3 is an in-memory S3 server that only accepts what Bunny Storage
// documents for its S3 API, and reproduces what a live zone showed: directory
// entries in listings sorted by their name without the trailing slash, empty
// directories left behind, recursive directory deletes, and long keys that are
// stored but never listed.
type fakeBunnyS3 struct {
	mu         sync.Mutex
	zone       string
	objects    map[string][]byte
	dirs       map[string]bool
	uploads    map[string]*fakeBunnyUpload
	reqs       []fakeBunnyReq
	violations []string

	errs        []int  // statuses to answer the next requests with
	retryAfter  string // sent with them
	corruptNext bool
	dirFirst    bool   // list a directory before the file of the same name, flipped by each listing
	query       string // overrides listing parameters, as in "prefix=&delimiter=/"

	page func([]fakeBunnyContent) []fakeBunnyContent // rewrites the listed keys
}

type fakeBunnyReq struct {
	method, key string
	header      http.Header
}

type fakeBunnyUpload struct {
	key   string
	parts map[int][]byte
	stale bool // a part was uploaded again
}

type fakeBunnyContent struct {
	Key, LastModified string
	Size              int
}

type fakeBunnyETagResult struct {
	XMLName xml.Name
	ETag    string
}

func newFakeBunnyS3(t *testing.T) (*fakeBunnyS3, ObjectStorage) {
	f := &fakeBunnyS3{zone: "jfs-fake", objects: map[string][]byte{}, dirs: map[string]bool{},
		uploads: map[string]*fakeBunnyUpload{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	s, err := CreateStorage("bunny-s3", srv.URL+"/"+f.zone+"?region=uk", "", "zone-password", "")
	if err != nil {
		t.Fatalf("create bunny-s3 storage: %s", err)
	}
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.violations) > 0 {
			t.Errorf("requests Bunny does not support: %q", f.violations)
		}
	})
	return f, s
}

func fakeBunnyETag(data []byte) string {
	sum := md5.Sum(data)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func (f *fakeBunnyS3) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *fakeBunnyS3) requests(method, key string) []fakeBunnyReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	var rs []fakeBunnyReq
	for _, r := range f.reqs {
		if r.method == method && r.key == key {
			rs = append(rs, r)
		}
	}
	return rs
}

func (f *fakeBunnyS3) writeXML(w http.ResponseWriter, status int, v any) {
	data, err := xml.Marshal(v)
	if err != nil {
		panic(err)
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func (f *fakeBunnyS3) fail(w http.ResponseWriter, status int, code, msg string) {
	f.writeXML(w, status, struct {
		XMLName       xml.Name `xml:"Error"`
		Code, Message string
	}{Code: code, Message: msg})
}

var fakeBunnyHeaders = map[string]bool{"x-amz-date": true, "x-amz-content-sha256": true, "x-amz-copy-source": true, "x-amz-copy-source-range": true}

// violation tells what Bunny does not document in a request.
func (f *fakeBunnyS3) violation(r *http.Request, key string) string {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential="+f.zone+"/") || !strings.Contains(auth, "/uk/s3/aws4_request") {
		return "credential scope is not <zone>/<date>/<region>/s3"
	}
	if v := r.Header.Get("X-Amz-Content-Sha256"); v != "UNSIGNED-PAYLOAD" {
		return "payload hash " + v
	}
	for name := range r.Header {
		switch n := strings.ToLower(name); {
		case n == "x-amz-checksum-sha256":
			if r.Method != http.MethodPut || r.URL.Query().Has("uploadId") || r.Header.Get("X-Amz-Copy-Source") != "" {
				return "x-amz-checksum-sha256 is only documented for PutObject"
			}
		case n == "content-encoding", strings.HasPrefix(n, "x-amz-") && !fakeBunnyHeaders[n]:
			return "header " + n
		}
	}
	if key == "" && (r.Method != http.MethodGet || r.URL.Query().Get("list-type") != "2") {
		return "bucket operation " + r.Method + " " + r.URL.RawQuery
	}
	return ""
}

func (f *fakeBunnyS3) store(key string, data []byte) {
	f.objects[key] = data
	for i := range key {
		if key[i] == '/' {
			f.dirs[key[:i+1]] = true
		}
	}
}

func (f *fakeBunnyS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	zone, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	q := r.URL.Query()
	f.reqs = append(f.reqs, fakeBunnyReq{r.Method, key, r.Header.Clone()})
	if zone != f.zone {
		f.fail(w, http.StatusNotFound, "NoSuchBucket", "The specified storage zone does not exist")
		return
	}
	if len(f.errs) > 0 {
		status := f.errs[0]
		f.errs = f.errs[1:]
		code := map[int]string{429: "SlowDown", 500: "InternalError", 503: "SlowDown"}[status]
		if code == "" {
			code = strings.ReplaceAll(http.StatusText(status), " ", "")
		}
		w.Header().Set("Retry-After", f.retryAfter)
		f.fail(w, status, code, "injected failure")
		return
	}
	if msg := f.violation(r, key); msg != "" {
		f.violations = append(f.violations, msg)
		f.fail(w, http.StatusBadRequest, "InvalidRequest", msg)
		return
	}
	if f.corruptNext && len(body) > 0 {
		f.corruptNext = false
		body[0] ^= 0xff
	}

	switch {
	case key == "":
		f.listV2(w, q)
	case r.Method == http.MethodPost && q.Has("uploads"):
		id := fmt.Sprintf("upload-%d", len(f.reqs))
		f.uploads[id] = &fakeBunnyUpload{key: key, parts: map[int][]byte{}}
		f.writeXML(w, http.StatusOK, struct {
			XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
			UploadId string
		}{UploadId: id})
	case r.Method == http.MethodPost && q.Has("uploadId"):
		f.completeUpload(w, key, q.Get("uploadId"), body)
	case r.Method == http.MethodPut && q.Has("uploadId"):
		f.uploadPart(w, r, key, q, body)
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		delete(f.uploads, q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") != "":
		if src, ok := f.copySource(w, r); ok {
			f.store(key, append([]byte(nil), f.objects[src]...))
			f.writeXML(w, http.StatusOK, fakeBunnyETagResult{xml.Name{Local: "CopyObjectResult"}, fakeBunnyETag(f.objects[key])})
		}
	case r.Method == http.MethodPut:
		if f.checkDigests(w, r, body) {
			f.store(key, body)
			w.WriteHeader(http.StatusOK)
		}
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		f.getObject(w, r, key)
	case r.Method == http.MethodDelete:
		// A live zone deletes a directory with everything below it, and
		// answers 204 even when nothing was deleted.
		for k := range f.objects {
			if k == key || strings.HasSuffix(key, "/") && strings.HasPrefix(k, key) {
				delete(f.objects, k)
			}
		}
		for d := range f.dirs {
			if strings.HasSuffix(key, "/") && strings.HasPrefix(d, key) {
				delete(f.dirs, d)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		f.violations = append(f.violations, r.Method+" "+key)
		f.fail(w, http.StatusNotImplemented, "NotImplemented", r.Method+" "+key)
	}
}

func (f *fakeBunnyS3) checkDigests(w http.ResponseWriter, r *http.Request, body []byte) bool {
	sha, md := sha256.Sum256(body), md5.Sum(body)
	for h, sum := range map[string][]byte{"X-Amz-Checksum-Sha256": sha[:], "Content-Md5": md[:]} {
		if v := r.Header.Get(h); v != "" && v != base64.StdEncoding.EncodeToString(sum) {
			f.fail(w, http.StatusBadRequest, "BadDigest", h+" mismatch")
			return false
		}
	}
	return true
}

func (f *fakeBunnyS3) getObject(w http.ResponseWriter, r *http.Request, key string) {
	data, ok := f.objects[key]
	if !ok {
		f.fail(w, http.StatusNotFound, "NoSuchKey", "The specified file does not exist")
		return
	}
	status := http.StatusOK
	if rg := r.Header.Get("Range"); rg != "" && r.Method == http.MethodGet {
		var start, end int
		n, _ := fmt.Sscanf(rg, "bytes=%d-%d", &start, &end)
		if n < 2 || end >= len(data) {
			end = len(data) - 1
		}
		if n < 1 || start > end {
			f.fail(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", rg)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		data, status = data[start:end+1], http.StatusPartialContent
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	w.WriteHeader(status)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

func (f *fakeBunnyS3) copySource(w http.ResponseWriter, r *http.Request) (string, bool) {
	src, err := url.PathUnescape(r.Header.Get("X-Amz-Copy-Source"))
	zone, key, _ := strings.Cut(src, "/")
	if _, ok := f.objects[key]; err != nil || zone != f.zone || !ok {
		f.fail(w, http.StatusNotFound, "NoSuchKey", "copy source does not exist")
		return "", false
	}
	return key, true
}

func (f *fakeBunnyS3) uploadPart(w http.ResponseWriter, r *http.Request, key string, q url.Values, body []byte) {
	u, ok := f.uploads[q.Get("uploadId")]
	var num int
	if _, err := fmt.Sscan(q.Get("partNumber"), &num); !ok || u.key != key || err != nil {
		f.fail(w, http.StatusNotFound, "NoSuchUpload", "upload does not exist")
		return
	}
	if r.Header.Get("X-Amz-Copy-Source") == "" {
		if f.checkDigests(w, r, body) {
			if _, ok := u.parts[num]; ok {
				u.stale = true
			}
			u.parts[num] = body
			w.Header().Set("ETag", fakeBunnyETag(body))
			w.WriteHeader(http.StatusOK)
		}
		return
	}
	src, ok := f.copySource(w, r)
	if !ok {
		return
	}
	var start, end int
	if _, err := fmt.Sscanf(r.Header.Get("X-Amz-Copy-Source-Range"), "bytes=%d-%d", &start, &end); err != nil || start > end || end >= len(f.objects[src]) {
		f.fail(w, http.StatusBadRequest, "InvalidArgument", "bad copy source range")
		return
	}
	u.parts[num] = append([]byte(nil), f.objects[src][start:end+1]...)
	f.writeXML(w, http.StatusOK, fakeBunnyETagResult{xml.Name{Local: "CopyPartResult"}, fakeBunnyETag(u.parts[num])})
}

func (f *fakeBunnyS3) completeUpload(w http.ResponseWriter, key, id string, body []byte) {
	u, ok := f.uploads[id]
	if !ok || u.key != key {
		f.fail(w, http.StatusNotFound, "NoSuchUpload", "upload does not exist")
		return
	}
	// Right after a part is uploaded again, a live zone still checks its
	// previous ETag for a moment.
	if u.stale {
		u.stale = false
		f.fail(w, http.StatusBadRequest, "InvalidPart", "One or more of the specified parts could not be found")
		return
	}
	var req struct {
		Parts []struct {
			PartNumber int
			ETag       string
		} `xml:"Part"`
	}
	if err := xml.Unmarshal(body, &req); err != nil || len(req.Parts) == 0 {
		f.fail(w, http.StatusBadRequest, "MalformedXML", "bad completion request")
		return
	}
	var data []byte
	for _, p := range req.Parts {
		part, ok := u.parts[p.PartNumber]
		if !ok || fakeBunnyETag(part) != `"`+strings.Trim(p.ETag, `"`)+`"` {
			f.fail(w, http.StatusBadRequest, "InvalidPart", fmt.Sprintf("part %d", p.PartNumber))
			return
		}
		data = append(data, part...)
	}
	delete(f.uploads, id)
	f.store(key, data)
	f.writeXML(w, http.StatusOK, fakeBunnyETagResult{xml.Name{Local: "CompleteMultipartUploadResult"}, fakeBunnyETag(data)})
}

var fakeBunnyEscape = strings.NewReplacer("%2F", "/", "+", "%20")

// listV2 lists like a live zone, an Elasticsearch search: entries, directories
// included, sort by their name without the trailing slash, a file and the
// directory of the same name in an order that changes between requests;
// start-after and continuation tokens resume after the sort value of the last
// entry, and common prefixes are only unique within a page.
func (f *fakeBunnyS3) listV2(w http.ResponseWriter, q url.Values) {
	over, _ := url.ParseQuery(f.query)
	maps.Copy(q, over)
	prefix, delim, after := q.Get("prefix"), q.Get("delimiter"), q.Get("start-after")
	if tok := q.Get("continuation-token"); tok != "" {
		after = tok
	}
	maxKeys := 1000
	if _, err := fmt.Sscan(q.Get("max-keys"), &maxKeys); err != nil || maxKeys > 1000 {
		maxKeys = 1000
	}
	value := func(k string) string { return strings.TrimSuffix(k, "/") }
	var keys []string
	for k := range f.objects {
		if !strings.HasSuffix(k, "/") && strings.HasPrefix(k, prefix) && utf8.RuneCountInString(k) <= 254-len(f.zone) {
			keys = append(keys, k)
		}
	}
	for d := range f.dirs {
		if strings.HasPrefix(value(d), prefix) {
			keys = append(keys, d)
		}
	}
	f.dirFirst = !f.dirFirst
	slices.SortFunc(keys, func(a, b string) int {
		if f.dirFirst && value(a) == value(b) {
			return strings.Compare(b, a)
		}
		return cmp.Or(strings.Compare(value(a), value(b)), strings.Compare(a, b))
	})

	var res struct {
		XMLName               xml.Name `xml:"ListBucketResult"`
		IsTruncated           bool
		NextContinuationToken string `xml:",omitempty"`
		EncodingType          string `xml:",omitempty"`
		Contents              []fakeBunnyContent
		CommonPrefixes        []struct{ Prefix string }
	}
	enc := func(k string) string { return k }
	if q.Get("encoding-type") == "url" {
		res.EncodingType = "url"
		enc = func(k string) string { return fakeBunnyEscape.Replace(url.QueryEscape(k)) }
	}
	seen := map[string]bool{}
	var n int
	for _, k := range keys {
		if after != "" && value(k) <= after {
			continue
		}
		var cp string
		if i := strings.Index(k[len(prefix):], delim); delim != "" && i >= 0 {
			cp = k[:len(prefix)+i+len(delim)]
		}
		if cp == "" || !seen[cp] {
			if n == maxKeys {
				res.IsTruncated = true
				break
			}
			if n++; cp == "" {
				res.Contents = append(res.Contents, fakeBunnyContent{enc(k), time.Now().UTC().Format(time.RFC3339), len(f.objects[k])})
			} else {
				seen[cp] = true
				res.CommonPrefixes = append(res.CommonPrefixes, struct{ Prefix string }{enc(cp)})
			}
		}
		res.NextContinuationToken = value(k)
	}
	if !res.IsTruncated {
		res.NextContinuationToken = ""
	}
	if f.page != nil {
		res.Contents = f.page(res.Contents)
	}
	f.writeXML(w, http.StatusOK, res)
}

// bunnyS3Refused tells whether an error comes from the wrapper, not from a
// request.
func bunnyS3Refused(err error) bool {
	var oe *smithy.OperationError
	return err != nil && !errors.As(err, &oe)
}

func TestBunnyS3Endpoint(t *testing.T) {
	cases := []struct {
		endpoint, accessKey string
		zone, region, base  string
	}{
		{"https://jfs-test.de-s3.storage.bunnycdn.com", "", "jfs-test", "de", "https://de-s3.storage.bunnycdn.com"},
		{"jfs-test.ny-s3.storage.bunnycdn.com", "", "jfs-test", "ny", "https://ny-s3.storage.bunnycdn.com"},
		{"https://JFS-Test.SYD-S3.storage.BunnyCDN.com/", "", "jfs-test", "syd", "https://syd-s3.storage.bunnycdn.com"},
		{"https://jh-s3.storage.bunnycdn.com/jfs-test", "", "jfs-test", "jh", "https://jh-s3.storage.bunnycdn.com"},
		{"https://uk-s3.storage.bunnycdn.com/jfs-test/?region=ny", "", "jfs-test", "uk", "https://uk-s3.storage.bunnycdn.com"},
		{"https://se-s3.storage.bunnycdn.com", "my-zone", "my-zone", "se", "https://se-s3.storage.bunnycdn.com"},
		{"http://127.0.0.1:9000/zone1?region=uk", "", "zone1", "uk", "http://127.0.0.1:9000"},
		{"http://127.0.0.1:9000/zone1", "", "zone1", "de", "http://127.0.0.1:9000"},
		{"https://de-s3.storage.bunnycdn.com", "", "", "", ""},
		{"https://jfs-test.de-s3.storage.bunnycdn.com/other", "", "", "", ""},
		{"https://de-s3.storage.bunnycdn.com/jfs-test/extra", "", "", "", ""},
		{"https://de-s3.storage.bunnycdn.com", "bad/zone", "", "", ""},
	}
	for _, c := range cases {
		s, err := CreateStorage("bunny-s3", c.endpoint, c.accessKey, "pw", "")
		if c.zone == "" {
			if err == nil {
				t.Errorf("%s: expected an error, got %s", c.endpoint, s)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %s", c.endpoint, err)
			continue
		}
		opts := s.(*bunnyS3).s3.Options()
		if s.String() != "bunny-s3://"+c.zone+"/" || opts.Region != c.region || aws.ToString(opts.BaseEndpoint) != c.base || !opts.UsePathStyle {
			t.Errorf("%s: got %s region %q endpoint %q path style %v", c.endpoint, s, opts.Region, aws.ToString(opts.BaseEndpoint), opts.UsePathStyle)
		}
	}
}

func TestBunnyS3Storage(t *testing.T) {
	_, s := newFakeBunnyS3(t)
	testStorage(t, s)
}

func TestBunnyS3UploadIntegrity(t *testing.T) {
	ctx := t.Context()
	f, s := newFakeBunnyS3(t)
	b64 := func(sum []byte) string { return base64.StdEncoding.EncodeToString(sum) }

	data := []byte("hello bunny")
	sum := sha256.Sum256(data)
	if err := s.Put(ctx, "k1", bytes.NewReader(data)); err != nil {
		t.Fatalf("put: %s", err)
	}
	if rs := f.requests(http.MethodPut, "k1"); len(rs) != 1 || rs[0].header.Get("X-Amz-Checksum-Sha256") != b64(sum[:]) {
		t.Fatalf("PutObject should carry x-amz-checksum-sha256 %s", b64(sum[:]))
	}
	seeked := bytes.NewReader([]byte("--payload"))
	_, _ = seeked.Seek(2, io.SeekStart)
	for k, r := range map[string]io.Reader{"seeked": seeked, "stream": io.MultiReader(strings.NewReader("pay"), strings.NewReader("load"))} {
		if err := s.Put(ctx, k, r); err != nil {
			t.Fatalf("put %s: %s", k, err)
		}
		if d, err := get(s, k, 0, -1); err != nil || d != "payload" {
			t.Fatalf("%s: got %q %v", k, d, err)
		}
	}
	f.set(func() { f.corruptNext = true })
	if err := s.Put(ctx, "k2", bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), "BadDigest") {
		t.Fatalf("a body corrupted in transit must be rejected, got %v", err)
	}
	if _, err := s.Head(ctx, "k2"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corrupted object must not be stored: %v", err)
	}

	up, err := s.CreateMultipartUpload(ctx, "mp")
	if err != nil {
		t.Fatal(err)
	}
	part := bytes.Repeat([]byte("p"), 5<<20)
	md := md5.Sum(part)
	if _, err := s.UploadPart(ctx, "mp", up.UploadID, 1, part); err != nil {
		t.Fatalf("upload part: %s", err)
	}
	if rs := f.requests(http.MethodPut, "mp"); len(rs) != 1 || rs[0].header.Get("Content-Md5") != b64(md[:]) {
		t.Fatal("UploadPart should carry Content-MD5")
	}
	f.set(func() { f.corruptNext = true })
	if _, err := s.UploadPart(ctx, "mp", up.UploadID, 2, part); err == nil || !strings.Contains(err.Error(), "BadDigest") {
		t.Fatalf("a part corrupted in transit must be rejected, got %v", err)
	}
	s.AbortUpload(ctx, "mp", up.UploadID)

	// The fake records a violation if the SDK adds its own checksums.
	t.Setenv("AWS_REQUEST_CHECKSUM_CALCULATION", "when_supported")
	t.Setenv("AWS_RESPONSE_CHECKSUM_VALIDATION", "when_supported")
	_, s = newFakeBunnyS3(t)
	if err := s.Put(ctx, "env", bytes.NewReader(data)); err != nil {
		t.Fatalf("put with AWS checksum env set: %s", err)
	}
	if d, err := get(s, "env", 0, -1); err != nil || d != string(data) {
		t.Fatalf("get with AWS checksum env set: %q %v", d, err)
	}
	if up, err = s.CreateMultipartUpload(ctx, "env-mp"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UploadPart(ctx, "env-mp", up.UploadID, 1, part); err != nil {
		t.Fatalf("upload part with AWS checksum env set: %s", err)
	}
}

func TestBunnyS3Copy(t *testing.T) {
	ctx := t.Context()
	_, s := newFakeBunnyS3(t)
	for _, key := range []string{"plain", "a+b%41 c", "dir/ünï cødé", "q=1&y#z:*|\\"} {
		data := []byte("data of " + key)
		if err := s.Put(ctx, key, bytes.NewReader(data)); err != nil {
			t.Fatalf("put %q: %s", key, err)
		}
		if err := s.Copy(ctx, key+"-copy", key); err != nil {
			t.Fatalf("copy %q: %s", key, err)
		}
		if d, err := get(s, key+"-copy", 0, -1); err != nil || d != string(data) {
			t.Fatalf("copy of %q: got %q %v", key, d, err)
		}
	}

	src := "big+%41 src"
	data := make([]byte, 6<<20)
	utils.RandRead(data)
	if err := s.Put(ctx, src, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	up, err := s.CreateMultipartUpload(ctx, "big copy")
	if err != nil {
		t.Fatal(err)
	}
	var parts []*Part
	for i, size := range []int64{5 << 20, 1 << 20} {
		p, err := s.UploadPartCopy(ctx, "big copy", up.UploadID, i+1, src, int64(i)*5<<20, size)
		if err != nil {
			t.Fatalf("upload part copy: %s", err)
		}
		parts = append(parts, p)
	}
	if err := s.CompleteUpload(ctx, "big copy", up.UploadID, parts); err != nil {
		t.Fatal(err)
	}
	if d, err := get(s, "big copy", 0, -1); err != nil || d != string(data) {
		t.Fatalf("part copy content mismatch: %v", err)
	}
}

func TestBunnyS3DirectoriesNeverDeleted(t *testing.T) {
	ctx := t.Context()
	f, s := newFakeBunnyS3(t)
	p := WithPrefix(s, "vol/")
	for store, keys := range map[ObjectStorage][]string{s: {"d/x", "d/y/z", "d/", "e/"}, p: {"dir/a", "dir/b/c"}} {
		for _, k := range keys {
			if err := store.Put(ctx, k, bytes.NewReader([]byte(k))); err != nil {
				t.Fatalf("put %s: %s", k, err)
			}
		}
	}
	for store, keys := range map[ObjectStorage][]string{s: {"d/", "d/y/", "e/", "missing/"}, p: {"dir/", "dir/b/"}} {
		for _, k := range keys {
			if err := store.Delete(ctx, k); err != nil {
				t.Fatalf("delete %s: %s", k, err)
			}
		}
	}
	for _, k := range []string{"d/x", "d/y/z", "vol/dir/a", "vol/dir/b/c"} {
		if d, err := get(s, k, 0, -1); err != nil || d != strings.TrimPrefix(k, "vol/") {
			t.Fatalf("%s is lost: %q %v", k, d, err)
		}
	}
	f.set(func() {
		for _, r := range f.reqs {
			if r.method == http.MethodDelete && strings.HasSuffix(r.key, "/") {
				t.Errorf("DELETE %s was sent: Bunny deletes a directory with its content", r.key)
			}
		}
	})
}

func TestBunnyS3List(t *testing.T) {
	ctx := t.Context()
	setup := func(t *testing.T, query string, page func([]fakeBunnyContent) []fakeBunnyContent, ks ...string) ObjectStorage {
		f, s := newFakeBunnyS3(t)
		for _, k := range ks {
			if err := s.Put(ctx, k, strings.NewReader(k)); err != nil {
				t.Fatalf("put %q: %s", k, err)
			}
		}
		f.set(func() { f.query, f.page = query, page })
		return s
	}
	special := []string{"l/A", "l/a+b", "l/a%41", "l/sp ace", "l/ünï", "l/a-c", "l/a/b", "l/a.b", "l/a0", "l/ctl\x1fchar"}

	// Raw XML cannot carry the control character.
	for query, ks := range map[string][]string{"": special, "encoding-type=": special[:len(special)-1]} {
		t.Run("special keys "+query, func(t *testing.T) {
			s := setup(t, query, nil, ks...)
			if got, want := bunnyS3ListPages(t, s, "l/", "", 1000, true), slices.Sorted(slices.Values(ks)); !slices.Equal(got, want) {
				t.Fatalf("listed %q, want %q", got, want)
			}
		})
	}

	for name, c := range map[string]struct {
		keys         []string
		start, query string
		page         func([]fakeBunnyContent) []fakeBunnyContent
	}{
		// Read as url-encoded, "l/c%41" would silently become "l/cA".
		"not url-encoded": {keys: []string{"l/a b", "l/c%41"}, page: func(cs []fakeBunnyContent) []fakeBunnyContent {
			for i := range cs {
				cs[i].Key, _ = url.PathUnescape(cs[i].Key)
			}
			return cs
		}},
		"out of order": {keys: special, page: func(cs []fakeBunnyContent) []fakeBunnyContent { slices.Reverse(cs); return cs }},
		"duplicated":   {keys: special, page: func(cs []fakeBunnyContent) []fakeBunnyContent { return slices.Insert(cs, 0, cs[0]) }},
		"duplicated directory": {keys: []string{"l/a", "l/a/"}, page: func(cs []fakeBunnyContent) []fakeBunnyContent {
			return append(cs, cs[slices.IndexFunc(cs, func(c fakeBunnyContent) bool { return strings.HasSuffix(c.Key, "/") })])
		}},
		"no progress":         {keys: special, page: func([]fakeBunnyContent) []fakeBunnyContent { return nil }},
		"outside the prefix":  {keys: []string{"k", "l/a"}, query: "prefix="},
		"start-after ignored": {keys: special, start: "l/a0", query: "start-after="},
		"common prefixes":     {keys: special, query: "delimiter=/"},
	} {
		t.Run(name, func(t *testing.T) {
			s := setup(t, c.query, c.page, c.keys...)
			for _, delim := range []string{"", "/"} {
				// A limit of two truncates the listing.
				if _, _, _, err := s.List(ctx, "l/", c.start, "", delim, 2, true); !bunnyS3Refused(err) {
					t.Fatalf("listing with delimiter %q must fail, got %v", delim, err)
				}
			}
		})
	}
}

// bunnyS3ListTree puts names around '/' in byte order, files named like
// directories, a directory of more than 1000 entries and empty directories,
// and returns the files and the directories that contain none.
func bunnyS3ListTree(t *testing.T, s ObjectStorage) (files, dirs []string) {
	ctx := t.Context()
	files = []string{"m/a+b c", "m/x%41", "m/sp ace", "m/d", "m/d a", "m/d-a", "m/d.a", "m/d/x", "m/d/y/z", "m/d0", "m/d0/q",
		"m/k", "m/k-1", "m/k/1", "m/e", "m/e-1", "m/u！", "m/u\U0001F600", "m/h/i", "b/big-a", "b/big.b", "b/big0", "b/big/sub/1"}
	for i := range 1005 {
		files = append(files, fmt.Sprintf("b/big/%04d", i))
	}
	var g errgroup.Group
	g.SetLimit(50)
	for _, k := range files {
		g.Go(func() error {
			if err := s.Put(ctx, k, strings.NewReader(k)); err != nil {
				return fmt.Errorf("put %q: %w", k, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	dirs = []string{"m/e/", "m/f/g/", "m/h/"}
	for _, d := range dirs[:2] {
		if err := s.Put(ctx, d, bytes.NewReader(nil)); err != nil {
			t.Fatalf("put %s: %s", d, err)
		}
	}
	if err := s.Delete(ctx, "m/h/i"); err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(files, func(k string) bool { return k == "m/h/i" }), dirs
}

// bunnyS3Fold groups keys as S3 lists them with a delimiter, in byte order.
func bunnyS3Fold(keys []string, prefix, delim string) []string {
	var want []string
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) || delim == "" && strings.HasSuffix(k, "/") {
			continue
		}
		if i := strings.Index(k[len(prefix):], delim); delim != "" && i >= 0 {
			k = k[:len(prefix)+i+1]
		}
		want = append(want, k)
	}
	slices.Sort(want)
	return slices.Compact(want)
}

// bunnyS3ListPages lists the way sync and ListAll do, after the last key, with
// or without the token.
func bunnyS3ListPages(t *testing.T, s ObjectStorage, prefix, delim string, limit int64, withToken bool) []string {
	var got []string
	for start, token := "", ""; ; {
		objs, more, next, err := s.List(t.Context(), prefix, start, token, delim, limit, true)
		if err != nil {
			t.Fatalf("list %q after %q with delimiter %q: %s", prefix, start, delim, err)
		}
		if len(objs) > int(limit) || more && (len(objs) == 0 || next == "") || !more && next != "" {
			t.Fatalf("list %q after %q: %d entries, more %v, token %q", prefix, start, len(objs), more, next)
		}
		for _, o := range objs {
			got = append(got, o.Key())
		}
		if !more {
			return got
		}
		start = objs[len(objs)-1].Key()
		if withToken {
			token = next
		}
	}
}

// bunnyS3CheckListing checks that every listing of the tree, with or without a
// delimiter, is the files in byte order, folded with the empty directories.
func bunnyS3CheckListing(t *testing.T, s ObjectStorage, files, dirs []string) {
	for _, c := range []struct {
		prefix string
		limits []int64
	}{{"m/", []int64{1, 2, 3, 1000}}, {"m/d", []int64{1, 2, 3}}, {"b/big/", []int64{1000}}, {"", []int64{3, 1000}}} {
		flat := bunnyS3ListPages(t, s, c.prefix, "", 1000, true)
		if want := bunnyS3Fold(files, c.prefix, ""); !slices.Equal(flat, want) {
			t.Fatalf("list %q:\n got %q\nwant %q", c.prefix, flat, want)
		}
		for _, delim := range []string{"", "/"} {
			want := bunnyS3Fold(append(slices.Clip(flat), dirs...), c.prefix, delim)
			for _, limit := range c.limits {
				for _, withToken := range []bool{true, false} {
					if got := bunnyS3ListPages(t, s, c.prefix, delim, limit, withToken); !slices.Equal(got, want) {
						t.Fatalf("list %q with delimiter %q by %d (token %v):\n got %q\nwant %q", c.prefix, delim, limit, withToken, got, want)
					}
				}
			}
		}
	}
}

func TestBunnyS3ListPaging(t *testing.T) {
	ctx := t.Context()
	f, s := newFakeBunnyS3(t)
	files, dirs := bunnyS3ListTree(t, s)
	bunnyS3CheckListing(t, s, files, dirs)

	// A listing by directory skips what is below a common prefix, here more
	// than 1000 keys below b/big/.
	for _, withToken := range []bool{true, false} {
		before := len(f.requests(http.MethodGet, ""))
		bunnyS3ListPages(t, s, "b/", "/", 3, withToken)
		if n := len(f.requests(http.MethodGet, "")) - before; n > 10 {
			t.Fatalf("listing b/ by directory took %d requests (token %v)", n, withToken)
		}
	}

	objs, _, next, err := s.List(ctx, "b/", "", "", "/", 3, true)
	if err != nil || len(objs) != 3 || objs[2].Key() != "b/big/" {
		t.Fatal(objs, err)
	}
	for _, m := range []struct{ prefix, start, token, delim string }{
		{"b/", "b/zz", next, "/"}, {"b/", "b/big/", next, ""}, {"b/big/", "b/big/", next, "/"},
		// A directory before the last key, which only a forged token carries.
		{"b/", "b/zz", `{"p":"b/","s":"/","l":"b/zz","a":"b/zz","d":["b/a/"]}`, "/"},
	} {
		if _, _, _, err := s.List(ctx, m.prefix, m.start, m.token, m.delim, 2, true); !bunnyS3Refused(err) {
			t.Fatalf("a token must only continue its own listing: %+v, got %v", m, err)
		}
	}

	// What a live zone returned for these keys, paged by one with continuation
	// tokens: "d/" twice, and before "d-a".
	_, s = newFakeBunnyS3(t)
	for _, k := range []string{"a+b c", "d-a", "d/x", "e/y"} {
		if err := s.Put(ctx, k, strings.NewReader(k)); err != nil {
			t.Fatal(err)
		}
	}
	raw := s.(*bunnyS3)
	var pages []string
	for token := ""; ; {
		in := &s3.ListObjectsV2Input{Bucket: &raw.bucket, Delimiter: aws.String("/"), MaxKeys: aws.Int32(1)}
		if token != "" {
			in.ContinuationToken = &token
		}
		resp, err := raw.s3.ListObjectsV2(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range resp.Contents {
			pages = append(pages, aws.ToString(o.Key))
		}
		for _, p := range resp.CommonPrefixes {
			pages = append(pages, aws.ToString(p.Prefix))
		}
		if token = aws.ToString(resp.NextContinuationToken); !aws.ToBool(resp.IsTruncated) {
			break
		}
	}
	if got := bunnyS3ListPages(t, s, "", "/", 1, true); strings.Join(pages, "|") != "a+b c|d/|d-a|d/|e/" ||
		strings.Join(got, "|") != "a+b c|d-a|d/|e/" {
		t.Fatalf("zone pages %q, listed %q", pages, got)
	}

	// Without a delimiter, a start ending with a slash is no common prefix.
	for _, k := range []string{"z/a/", "z/b/", "z/c/", "z/d/x"} {
		if err := s.Put(ctx, k, strings.NewReader(k)); err != nil {
			t.Fatal(err)
		}
	}
	if objs, _, _, err := s.List(ctx, "", "z/", "", "", 1, true); err != nil || len(objs) != 1 || objs[0].Key() != "z/d/x" {
		t.Fatalf("listed %v after z/: %v", objs, err)
	}
}

func TestBunnyS3KeyNames(t *testing.T) {
	ctx := t.Context()
	f, s := newFakeBunnyS3(t)
	longest := strings.Repeat("é", 254-len(f.zone))
	if err := s.Put(ctx, longest, bytes.NewReader([]byte("v"))); err != nil {
		t.Fatalf("put a %d-character key: %s", utf8.RuneCountInString(longest), err)
	}
	if objs, _, _, err := s.List(ctx, "", "", "", "", 10, true); err != nil || len(objs) != 1 {
		t.Fatalf("the longest allowed key must be listed: %d %v", len(objs), err)
	}
	up, err := s.CreateMultipartUpload(ctx, "mp")
	if err != nil {
		t.Fatal(err)
	}
	ops := map[string]func(key string) error{
		"put":       func(k string) error { return s.Put(ctx, k, bytes.NewReader([]byte("v"))) },
		"copy to":   func(k string) error { return s.Copy(ctx, k, longest) },
		"copy from": func(k string) error { return s.Copy(ctx, "c", k) },
		"multipart": func(k string) error { _, err := s.CreateMultipartUpload(ctx, k); return err },
		"part copy": func(k string) error { _, err := s.UploadPartCopy(ctx, "mp", up.UploadID, 1, k, 0, 1); return err },
		"head":      func(k string) error { _, err := s.Head(ctx, k); return err },
		"get":       func(k string) error { _, err := s.Get(ctx, k, 0, -1); return err },
		"delete":    func(k string) error { return s.Delete(ctx, k) },
	}
	// Bunny hides the first key from listings, rejects the next two, and
	// drops `"<>` and cuts names at `?`, which would reach another object.
	for _, key := range []string{longest + "x", "dir/ leading", "dir/trailing ", `a"b`, "a<b", "a>b", "a?b"} {
		for name, op := range ops {
			var sent int
			f.set(func() { sent = -len(f.reqs) })
			err := op(key)
			f.set(func() { sent += len(f.reqs) })
			if err == nil || sent > 0 {
				t.Errorf("%s %q must be refused without a request, got %v", name, key, err)
			}
		}
	}
}

func TestBunnyS3Tiers(t *testing.T) {
	ctx := t.Context()
	_, s := newFakeBunnyS3(t)
	st := s.(SupportTier)
	tiers := NewTiers("")
	tiers[1] = Tier{ID: 1}
	if err := st.InitTiers(tiers); err != nil {
		t.Fatalf("tiers without storage class or tag: %s", err)
	}
	for _, tier := range []Tier{{ID: 1, Sc: "STANDARD_IA"}, {ID: 1, Tag: "a=b"}} {
		tiers := NewTiers("")
		tiers[1] = tier
		if err := st.InitTiers(tiers); !errors.Is(err, notSupported) {
			t.Fatalf("tier %+v should not be supported: %v", tier, err)
		}
	}
	tctx := context.WithValue(ctx, TierKey{}, uint8(1))
	if err := s.Put(tctx, "k", bytes.NewReader([]byte("v"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMultipartUpload(tctx, "mp"); err != nil {
		t.Fatal(err)
	}
	if err := s.Copy(tctx, "k2", "k"); err != nil {
		t.Fatal(err)
	}
}

func TestBunnyS3CreateAndRestore(t *testing.T) {
	ctx := t.Context()
	f, s := newFakeBunnyS3(t)
	if err := s.Create(ctx); err != nil {
		t.Fatalf("create an existing zone: %s", err)
	}
	other, err := CreateStorage("bunny-s3", aws.ToString(s.(*bunnyS3).s3.Options().BaseEndpoint)+"/no-such-zone", "", "pw", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Create(ctx); err == nil || !strings.Contains(err.Error(), "S3 compatibility") {
		t.Fatalf("create should point at S3 compatibility, got %v", err)
	}
	if err := s.Restore(ctx, "k", 1); !errors.Is(err, notSupported) {
		t.Fatalf("restore should not be supported: %v", err)
	}
	if n := len(f.requests(http.MethodPost, "k")); n != 0 {
		t.Fatal("RestoreObject was sent")
	}
}

func TestBunnyS3Retries(t *testing.T) {
	ctx := t.Context()
	t.Setenv("AWS_MAX_ATTEMPTS", "10") // must not raise the attempts
	f, s := newFakeBunnyS3(t)

	// Without Retry-After, the first retry comes within 2 seconds.
	f.set(func() { f.errs, f.retryAfter = []int{503}, "2" })
	start := time.Now()
	if err := s.Put(ctx, "k", bytes.NewReader([]byte("v"))); err != nil || time.Since(start) < 2*time.Second {
		t.Fatalf("Retry-After was not honoured: %v after %s", err, time.Since(start))
	}

	for key, c := range map[string]struct {
		errs     []int
		attempts int
	}{
		"slowdown":        {[]int{503, 503, 503, 503}, 4},
		"internal":        {[]int{500, 500, 500, 500}, 4},
		"transient":       {[]int{429, 502, 504}, 4},
		"bad request":     {[]int{400}, 1},
		"forbidden":       {[]int{403}, 1},
		"not found":       {[]int{404}, 1},
		"conflict":        {[]int{409}, 1},
		"not implemented": {[]int{501}, 1},
	} {
		f.set(func() { f.errs, f.retryAfter = c.errs, "0" })
		err := s.Put(ctx, key, bytes.NewReader([]byte("v")))
		if n := len(f.requests(http.MethodPut, key)); n != c.attempts {
			t.Errorf("%s: expected %d attempts, got %d", key, c.attempts, n)
		}
		// The error of the last attempt reaches the caller.
		var re interface{ HTTPStatusCode() int }
		if fails := c.attempts <= len(c.errs); fails != (err != nil) || fails && (!errors.As(err, &re) || re.HTTPStatusCode() != c.errs[c.attempts-1]) {
			t.Errorf("%s: put returned %v", key, err)
		}
	}

	// Unlike with the SDK's retry quota, throttling never uses the retries up.
	for i := range 60 {
		f.set(func() { f.errs, f.retryAfter = []int{503, 503, 503}, "0" })
		if err := s.Put(ctx, "k", bytes.NewReader(nil)); err != nil {
			t.Fatalf("put %d: %s", i, err)
		}
	}

	// Every attempt sends the whole body from where the caller left it, as the
	// fake checks it against its checksum.
	data, part := []byte("retried payload"), bytes.Repeat([]byte("p"), 5<<20)
	up, err := s.CreateMultipartUpload(ctx, "mp")
	if err != nil {
		t.Fatal(err)
	}
	var p *Part
	for key, op := range map[string]func() error{
		"k500": func() error {
			r := bytes.NewReader(append([]byte("--"), data...))
			_, _ = r.Seek(2, io.SeekStart)
			return s.Put(ctx, "k500", r)
		},
		"mp": func() (err error) { p, err = s.UploadPart(ctx, "mp", up.UploadID, 1, part); return },
	} {
		f.set(func() { f.errs = []int{500} })
		if err := op(); err != nil || len(f.requests(http.MethodPut, key)) != 2 {
			t.Fatalf("%s: an InternalError must be retried once: %v", key, err)
		}
	}
	if err := s.CompleteUpload(ctx, "mp", up.UploadID, []*Part{p}); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string][]byte{"k500": data, "mp": part} {
		if d, err := get(s, key, 0, -1); err != nil || d != string(want) {
			t.Fatalf("%s is not what was uploaded: %v", key, err)
		}
	}

	// Right after a part is uploaded again, the fake answers InvalidPart once.
	if up, err = s.CreateMultipartUpload(ctx, "mp"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if p, err = s.UploadPart(ctx, "mp", up.UploadID, 1, part); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CompleteUpload(ctx, "mp", up.UploadID, []*Part{p}); err != nil {
		t.Fatalf("an overwritten part must be accepted once Bunny catches up: %s", err)
	}

	h := http.Header{}
	h.Set("Retry-After", "60")
	resp := &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: &http.Response{StatusCode: 503, Header: h}}, Err: errors.New("x")}
	if d, _ := newBunnyS3Retryer().RetryDelay(1, resp); d != bunnyS3MaxBackoff {
		t.Errorf("Retry-After should be capped at %s, got %s", bunnyS3MaxBackoff, d)
	}
}

type bunnyS3LivePrefix struct {
	ObjectStorage
	prefix string
}

func (p *bunnyS3LivePrefix) Copy(ctx context.Context, dst, src string) error {
	return p.ObjectStorage.Copy(ctx, p.prefix+dst, p.prefix+src)
}

// TestBunnyS3Live runs against a real storage zone created with S3 compatibility.
// BUNNY_S3_BUCKET is the --bucket URL, BUNNY_S3_SECRET_KEY the zone password.
func TestBunnyS3Live(t *testing.T) { //skip mutate
	bucket, secret := os.Getenv("BUNNY_S3_BUCKET"), os.Getenv("BUNNY_S3_SECRET_KEY")
	if bucket == "" || secret == "" {
		t.SkipNow()
	}
	ctx := context.Background()
	b, err := newBunnyS3(bucket, os.Getenv("BUNNY_S3_ACCESS_KEY"), secret, "")
	if err != nil {
		t.Fatal(err)
	}
	raw := b.(*bunnyS3)
	run := fmt.Sprintf("jfs-live-%d-%04d/", time.Now().Unix(), rand.Intn(10000))
	s := WithPrefix(raw, run)
	t.Logf("using prefix %s", run)
	t.Cleanup(func() {
		// Bunny deletes the directory of this run with everything below it.
		if _, err := raw.s3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &raw.bucket, Key: &run}); err != nil {
			t.Errorf("delete %s: %s", run, err)
		}
		if left, err := listAll(ctx, raw, run, "", 10, true); err != nil || len(left) > 0 {
			t.Errorf("leftovers under %s: %d %v", run, len(left), err)
		}
	})

	t.Run("storage", func(t *testing.T) {
		testStorage(t, &bunnyS3LivePrefix{s, run})
	})

	t.Run("dir delete keeps children", func(t *testing.T) {
		for _, k := range []string{"d/x", "d/y/z"} {
			if err := s.Put(ctx, k, bytes.NewReader([]byte(k))); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Delete(ctx, "d/"); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"d/x", "d/y/z"} {
			if d, err := get(s, k, 0, -1); err != nil || d != k {
				t.Fatalf("%s is lost: %v", k, err)
			}
		}
	})

	t.Run("special keys", func(t *testing.T) {
		ks := []string{"k/A", "k/a+b", "k/a%41", "k/sp ace", "k/ünï", "k/a-c", "k/a/b", "k/a.b", "k/a0"}
		for _, k := range ks {
			if err := s.Put(ctx, k, bytes.NewReader([]byte(k))); err != nil {
				t.Fatalf("put %q: %s", k, err)
			}
		}
		slices.Sort(ks)
		objs, err := listAll(ctx, s, "k/", "", 100, true)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, o := range objs {
			got = append(got, o.Key())
		}
		if strings.Join(got, "|") != strings.Join(ks, "|") {
			t.Fatalf("listed %q, want %q", got, ks)
		}
		if err := s.Copy(ctx, run+"k/copy", run+"k/a+b"); err != nil {
			t.Fatalf("copy a special key: %s", err)
		}
		if d, err := get(s, "k/copy", 0, -1); err != nil || d != "k/a+b" {
			t.Fatalf("copy content: %q %v", d, err)
		}
		for _, k := range []string{`k/a"b`, "k/a<b", "k/a?b", "k/" + strings.Repeat("x", 250)} {
			if err := s.Put(ctx, k, bytes.NewReader([]byte("v"))); err == nil {
				t.Fatalf("put %q must be refused", k)
			}
		}
	})

	t.Run("list pages", func(t *testing.T) {
		ls := WithPrefix(s, "list/")
		files, dirs := bunnyS3ListTree(t, ls)
		bunnyS3CheckListing(t, ls, files, dirs)
	})

	t.Run("corrupted upload rejected", func(t *testing.T) {
		key := run + "bad-digest"
		wrong := sha256.Sum256([]byte("something else"))
		_, err := raw.s3.PutObject(ctx, &s3.PutObjectInput{Bucket: &raw.bucket, Key: &key, Body: bytes.NewReader([]byte("data")),
			ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(wrong[:]))})
		if err == nil || !strings.Contains(err.Error(), "BadDigest") {
			t.Fatalf("a wrong x-amz-checksum-sha256 must be rejected, got %v", err)
		}
		if _, err := raw.Head(ctx, key); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the rejected object exists: %v", err)
		}
	})
}

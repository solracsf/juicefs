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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/juicedata/juicefs/pkg/utils"
)

const (
	bunnyS3MaxAttempts = 4
	bunnyS3MaxBackoff  = 10 * time.Second
)

var (
	bunnyS3Host       = regexp.MustCompile(`^(?:([a-z0-9-]+)\.)?([a-z]+)-s3\.storage\.bunnycdn\.com$`)
	bunnyS3Zone       = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	bunnyS3CopyEscape = strings.NewReplacer("%2F", "/", "+", "%20")
)

type bunnyS3 struct {
	s3client
}

func (s *bunnyS3) String() string {
	return fmt.Sprintf("bunny-s3://%s/", s.bucket)
}

// InitTiers refuses storage classes and tags, which Bunny ignores.
func (s *bunnyS3) InitTiers(init Tiers) error {
	for _, t := range init {
		if t.Sc != "" || t.Tag != "" {
			return notSupported
		}
	}
	return nil
}

func (s *bunnyS3) Create(ctx context.Context) error {
	if _, _, _, err := s.List(ctx, "", "", "", "", 1, true); err != nil {
		return fmt.Errorf("storage zone %s is not reachable through the S3 API; create it in the Bunny dashboard with S3 compatibility enabled (it cannot be enabled later): %w", s.bucket, err)
	}
	return nil
}

// checkKey refuses keys that a live zone maps to another object, as it drops
// `"`, `<` and `>` from names and cuts them at `?`; keys it stores but never
// lists (a zone named jfs-test listed keys of up to 246 characters, taken as
// 255 with "<zone>/"); and names starting or ending with a space, which it
// answers with 500.
func (s *bunnyS3) checkKey(key string) error {
	if strings.ContainsAny(key, `"<>?`) {
		return fmt.Errorf("invalid key %q: Bunny changes names containing \", <, > or ?", key)
	}
	limit := 254 - len(s.bucket)
	if limit > 246 {
		limit = 246
	}
	if n := utf8.RuneCountInString(key); n > limit {
		return fmt.Errorf("key %q is %d characters long, %s does not list keys longer than %d characters", key, n, s, limit)
	}
	if name := key[strings.LastIndex(key, "/")+1:]; strings.HasPrefix(name, " ") || strings.HasSuffix(name, " ") {
		return fmt.Errorf("invalid key %q: Bunny does not accept names starting or ending with a space", key)
	}
	return nil
}

func (s *bunnyS3) copySource(key string) *string {
	return aws.String(bunnyS3CopyEscape.Replace(url.QueryEscape(s.bucket + "/" + key)))
}

func (s *bunnyS3) Head(ctx context.Context, key string) (Object, error) {
	if err := s.checkKey(key); err != nil {
		return nil, err
	}
	return s.s3client.Head(ctx, key)
}

func (s *bunnyS3) Get(ctx context.Context, key string, off, limit int64, getters ...AttrGetter) (io.ReadCloser, error) {
	if err := s.checkKey(key); err != nil {
		return nil, err
	}
	return s.s3client.Get(ctx, key, off, limit, getters...)
}

// Put sends the SHA-256 of the body, so that Bunny rejects corrupted uploads.
func (s *bunnyS3) Put(ctx context.Context, key string, in io.Reader, getters ...AttrGetter) error {
	if err := s.checkKey(key); err != nil {
		return err
	}
	body, ok := in.(io.ReadSeeker)
	if !ok {
		data, err := io.ReadAll(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	pos, err := body.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err = io.Copy(h, body); err != nil {
		return err
	}
	if _, err = body.Seek(pos, io.SeekStart); err != nil {
		return err
	}
	_, err = s.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:         &s.bucket,
		Key:            &key,
		Body:           body,
		ContentType:    aws.String(utils.GuessMimeType(key)),
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(h.Sum(nil))),
	})
	return err
}

func (s *bunnyS3) Copy(ctx context.Context, dst, src string) error {
	if err := s.checkKey(dst); err != nil {
		return err
	}
	if err := s.checkKey(src); err != nil {
		return err
	}
	_, err := s.s3.CopyObject(ctx, &s3.CopyObjectInput{Bucket: &s.bucket, Key: &dst, CopySource: s.copySource(src)})
	return err
}

func (s *bunnyS3) Delete(ctx context.Context, key string, getters ...AttrGetter) error {
	if err := s.checkKey(key); err != nil {
		return err
	}
	// A live zone deletes a directory with everything below it, including keys
	// that it does not list, so a "dir/" key is never deleted.
	if strings.HasSuffix(key, "/") {
		return nil
	}
	return s.s3client.Delete(ctx, key, getters...)
}

// bunnyS3ListCursor is the token of List: the listing, the key it returned
// last, the sort value to list after, and the directories seen but not
// returned yet.
type bunnyS3ListCursor struct {
	Prefix    string   `json:"p"`
	Delimiter string   `json:"s"`
	Last      string   `json:"l"`
	After     string   `json:"a,omitempty"`
	Dirs      []string `json:"d,omitempty"`
}

// bunnyS3SkipDir, appended to a common prefix, sorts after every key below it.
const bunnyS3SkipDir = "\U0010FFFF"

// bunnyS3SortValue is what a live zone sorts a listing by, and compares
// start-after and continuation tokens with: a directory sorts as its name
// without the trailing slash, in either order with a file of that name.
func bunnyS3SortValue(key string) string {
	return strings.TrimSuffix(key, "/")
}

func bunnyS3DirObj(key string) *obj {
	return &obj{key, 0, time.Unix(0, 0), true, "", ""}
}

// newBunnyS3ListCursor lists after start without a token. The directories
// named like start up to a byte below '/' sort after start but are listed
// before it, so it lists again from the first of them.
func newBunnyS3ListCursor(prefix, start, delimiter string) *bunnyS3ListCursor {
	c := &bunnyS3ListCursor{Prefix: prefix, Delimiter: delimiter, Last: start}
	if start < prefix {
		return c
	}
	if !strings.HasPrefix(start, prefix) {
		return nil
	}
	c.After = start
	if delimiter == "" {
		return c
	}
	name, _, _ := strings.Cut(start[len(prefix):], "/")
	lo := 1
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		lo = 0
	}
	for i := lo; i <= len(name); i++ {
		if i == len(name) || name[i] < '/' {
			v := prefix + name[:i]
			_, size := utf8.DecodeLastRuneInString(v)
			c.After = v[:len(v)-size]
			break
		}
	}
	return c
}

// List returns keys and common prefixes in byte order, each once. A live zone
// does not: continuation tokens repeat common prefixes, and like start-after
// they skip whatever shares the sort value of the last entry. So List pages
// with start-after only, never between two entries of one sort value, lists
// without a delimiter and groups common prefixes itself, holding directories
// back until their place in byte order. It fails on a listing it cannot trust.
func (s *bunnyS3) List(ctx context.Context, prefix, start, token, delimiter string, limit int64, followLink bool) ([]Object, bool, string, error) {
	if delimiter != "" && delimiter != "/" {
		return nil, false, "", notSupported
	}
	if limit = max(limit, 1); limit > 1000 {
		limit = 1000
	}
	c := newBunnyS3ListCursor(prefix, start, delimiter)
	if token != "" {
		c = new(bunnyS3ListCursor)
		if json.Unmarshal([]byte(token), c) != nil || c.Prefix != prefix || c.Delimiter != delimiter || start != "" && start != c.Last {
			return nil, false, "", fmt.Errorf("invalid token %q after %q when listing %s with prefix %q", token, start, s, prefix)
		}
	}
	if c == nil {
		return nil, false, "", nil
	}
	fail := func(format string, args ...any) ([]Object, bool, string, error) {
		return nil, false, "", fmt.Errorf("listing %s with prefix %q after %q: %s", s, prefix, c.Last, fmt.Sprintf(format, args...))
	}
	var objs []Object
	last, after, dirs := c.Last, c.After, c.Dirs
	full, more, disorder := false, true, ""
	// Everything List returns goes through add, which checks the byte order
	// even when the page is full.
	add := func(o *obj) bool {
		if o.key <= last {
			disorder = o.key
		}
		if full = len(objs) == int(limit); !full {
			objs, last = append(objs, o), o.key
		}
		return !full
	}
	// Without a delimiter, a page of entries ends the call, even if some were
	// directories.
	for more && len(objs) < int(limit) && (delimiter != "" || len(objs) == 0) {
		// Three entries span two sort values or more, so a page makes progress.
		resp, err := s.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &prefix, StartAfter: &after,
			MaxKeys: aws.Int32(int32(max(limit, 3))), EncodingType: types.EncodingTypeUrl})
		if err != nil {
			return nil, false, "", err
		}
		if len(resp.CommonPrefixes) > 0 {
			return fail("common prefixes without a delimiter")
		}
		keys := make([]string, len(resp.Contents))
		for i, o := range resp.Contents {
			key := aws.ToString(o.Key)
			if resp.EncodingType != "" {
				if strings.ContainsFunc(key, func(r rune) bool { return r <= ' ' || r >= 0x7f || r == '+' }) {
					return fail("listed key %q is not url-encoded", key)
				}
				if key, err = url.PathUnescape(key); err != nil {
					return fail("%s", err)
				}
			}
			v, prev := bunnyS3SortValue(key), after
			if i > 0 {
				prev = bunnyS3SortValue(keys[i-1])
			}
			// Only a file and the directory of the same name share a sort value.
			if !strings.HasPrefix(key, prefix) || v < prev || v == prev && (i == 0 || key == keys[i-1] || i > 1 && key == keys[i-2]) {
				return fail("key %q is invalid or out of order", key)
			}
			keys[i] = key
		}
		n := len(keys)
		if more = aws.ToBool(resp.IsTruncated); more {
			// The other entry of the last sort value can be on the next page.
			for n > 0 && bunnyS3SortValue(keys[n-1]) == bunnyS3SortValue(keys[len(keys)-1]) {
				n--
			}
			if n == 0 {
				return fail("no progress")
			}
		}
		for i, key := range keys[:n] {
			level := key[len(prefix):]
			d := strings.Index(level, "/")
			var e *obj
			switch {
			case strings.HasSuffix(key, "/") && delimiter == "":
			case strings.HasSuffix(key, "/") && d == len(level)-1:
				if j, found := slices.BinarySearch(dirs, key); !found && key > last {
					dirs = slices.Insert(dirs, j, key)
				}
			case delimiter != "" && d >= 0:
				e = bunnyS3DirObj(key[:len(prefix)+d+1])
			default:
				o := resp.Contents[i]
				e = &obj{key, aws.ToInt64(o.Size), aws.ToTime(o.LastModified), false,
					getOrDefaultScValue(string(o.StorageClass), string(types.ObjectStorageClassStandard)), ""}
			}
			if e != nil {
				for len(dirs) > 0 && dirs[0] <= e.key && (dirs[0] == e.key || add(bunnyS3DirObj(dirs[0]))) {
					dirs = dirs[1:]
				}
				// Keys up to c.Last were returned before, and a common prefix
				// once for all the keys below it.
				if full || e.key > c.Last && (e.key != last || !e.isDir) && !add(e) {
					break
				}
			}
			if i+1 == n || bunnyS3SortValue(keys[i+1]) != bunnyS3SortValue(key) {
				after = bunnyS3SortValue(key)
			}
		}
		if delimiter != "" && strings.HasSuffix(last, "/") && strings.HasPrefix(after, last) {
			after = max(after, last+bunnyS3SkipDir)
		}
	}
	for !more && len(dirs) > 0 && add(bunnyS3DirObj(dirs[0])) {
		dirs = dirs[1:]
	}
	if disorder != "" {
		return fail("%q returned out of order", disorder)
	}
	if !more && !full {
		return objs, false, "", nil
	}
	next, err := json.Marshal(bunnyS3ListCursor{prefix, delimiter, last, after, dirs})
	return objs, true, string(next), err
}

func (s *bunnyS3) CreateMultipartUpload(ctx context.Context, key string) (*MultipartUpload, error) {
	if err := s.checkKey(key); err != nil {
		return nil, err
	}
	return s.s3client.CreateMultipartUpload(ctx, key)
}

// UploadPart sends the MD5 of the part, so that Bunny rejects corrupted parts.
func (s *bunnyS3) UploadPart(ctx context.Context, key string, uploadID string, num int, body []byte) (*Part, error) {
	sum := md5.Sum(body)
	resp, err := s.s3.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     &s.bucket,
		Key:        &key,
		UploadId:   &uploadID,
		Body:       bytes.NewReader(body),
		PartNumber: aws.Int32(int32(num)),
		ContentMD5: aws.String(base64.StdEncoding.EncodeToString(sum[:])),
	})
	if err != nil {
		return nil, err
	}
	return &Part{Num: num, ETag: aws.ToString(resp.ETag)}, nil
}

func (s *bunnyS3) UploadPartCopy(ctx context.Context, key string, uploadID string, num int, srcKey string, off, size int64) (*Part, error) {
	if err := s.checkKey(srcKey); err != nil {
		return nil, err
	}
	resp, err := s.s3.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
		Bucket:          &s.bucket,
		CopySource:      s.copySource(srcKey),
		CopySourceRange: aws.String(fmt.Sprintf("bytes=%d-%d", off, off+size-1)),
		Key:             &key,
		PartNumber:      aws.Int32(int32(num)),
		UploadId:        &uploadID,
	})
	if err != nil {
		return nil, err
	}
	if resp.CopyPartResult == nil {
		return nil, fmt.Errorf("bunny-s3: UploadPartCopy returned no CopyPartResult for %s part %d", key, num)
	}
	return &Part{Num: num, ETag: aws.ToString(resp.CopyPartResult.ETag)}, nil
}

func (s *bunnyS3) Restore(ctx context.Context, key string, days int32) error {
	return notSupported
}

// newBunnyS3Retryer retries only throttling and transient server errors, which
// a live zone answered some of 500 concurrent uploads with, honoring
// Retry-After, and CompleteMultipartUpload failing with InvalidPart: right
// after a part is uploaded again, a live zone still checks its previous ETag
// for a moment.
func newBunnyS3Retryer() aws.Retryer {
	jitter := retry.NewExponentialJitterBackoff(bunnyS3MaxBackoff)
	return retry.NewStandard(func(o *retry.StandardOptions) {
		o.MaxAttempts = bunnyS3MaxAttempts
		o.RateLimiter = ratelimit.None
		o.Retryables = []retry.IsErrorRetryable{
			retry.RetryableHTTPStatusCode{Codes: map[int]struct{}{http.StatusTooManyRequests: {}, http.StatusInternalServerError: {},
				http.StatusBadGateway: {}, http.StatusServiceUnavailable: {}, http.StatusGatewayTimeout: {}}},
			retry.RetryableErrorCode{Codes: map[string]struct{}{"InvalidPart": {}}},
		}
		o.Backoff = retry.BackoffDelayerFunc(func(attempt int, err error) (time.Duration, error) {
			var ae smithy.APIError
			if errors.As(err, &ae) && ae.ErrorCode() == "InvalidPart" {
				return time.Second << (attempt - 1), nil
			}
			var re *smithyhttp.ResponseError
			if errors.As(err, &re) {
				if secs, err := strconv.Atoi(re.Response.Header.Get("Retry-After")); err == nil && secs >= 0 {
					return min(time.Duration(secs)*time.Second, bunnyS3MaxBackoff), nil
				}
			}
			return jitter.BackoffDelay(attempt, err)
		})
	})
}

// newBunnyS3 accepts https://<zone>.<region>-s3.storage.bunnycdn.com,
// https://<region>-s3.storage.bunnycdn.com/<zone> and, with the zone taken from
// the access key, https://<region>-s3.storage.bunnycdn.com. Other hosts (proxies,
// tests) take the region from ?region=, or "de".
func newBunnyS3(endpoint, accessKey, secretKey, token string) (ObjectStorage, error) {
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	uri, err := url.ParseRequestURI(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint %s: %s", endpoint, err)
	}
	host, region, zone := strings.ToLower(uri.Host), cmp.Or(uri.Query().Get("region"), "de"), strings.Trim(uri.Path, "/")
	var hostZone string
	if m := bunnyS3Host.FindStringSubmatch(host); m != nil {
		hostZone, region, host = m[1], m[2], m[2]+"-s3.storage.bunnycdn.com"
	}
	if hostZone != "" && zone != "" {
		return nil, fmt.Errorf("invalid endpoint %s: the storage zone is given both in the host and in the path", endpoint)
	}
	if zone = cmp.Or(hostZone, zone, accessKey); !bunnyS3Zone.MatchString(zone) {
		return nil, fmt.Errorf("invalid endpoint %s: invalid or missing storage zone name %q", endpoint, zone)
	}
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cmp.Or(accessKey, zone), secretKey, token)))
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %s", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.Region = region
		o.BaseEndpoint = aws.String(uri.Scheme + "://" + host)
		o.UsePathStyle = defaultPathStyle()
		o.HTTPClient = httpClient
		// Bunny only accepts the SHA-256 and MD5 checksums set above, whatever
		// AWS_REQUEST_CHECKSUM_CALCULATION says.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		o.APIOptions = append(o.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
		o.RetryMaxAttempts = 0
		o.Retryer = newBunnyS3Retryer()
	})
	return &bunnyS3{s3client{bucket: zone, s3: client, region: region, disableChecksum: true}}, nil
}

func init() {
	Register("bunny-s3", newBunnyS3)
}

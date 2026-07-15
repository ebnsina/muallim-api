package blob

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Options configures an S3-compatible store.
//
// Cloudflare R2 in production, MinIO in a container for development. They speak
// the same protocol, so there is one implementation and one code path — a
// development store that behaves differently from the production one is a
// development store that hides the bug you are about to ship.
type Options struct {
	// Endpoint is the S3 API address. For R2 that is
	// https://<account>.r2.cloudflarestream.com; for MinIO, http://localhost:9000.
	Endpoint string

	Bucket    string
	AccessKey string
	SecretKey string

	// Region. R2 ignores it but SigV4 signs it, so it must match on both sides.
	// `auto` is what R2 documents.
	Region string

	// PathStyle addresses a bucket as `endpoint/bucket/key` rather than
	// `bucket.endpoint/key`. MinIO needs it; R2 accepts it.
	PathStyle bool
}

// S3 is a Store backed by anything that speaks the S3 API.
type S3 struct {
	client  *s3.Client
	presign *s3.PresignClient
	bucket  string
}

// NewS3 returns a store, and refuses one it cannot use.
//
// The credentials are static and passed in, rather than discovered by the SDK's
// default chain. A process that silently picks up an instance role, or a stray
// ~/.aws/credentials, is a process whose bucket you cannot reason about.
func NewS3(opts Options) (*S3, error) {
	switch {
	case opts.Endpoint == "":
		return nil, errors.New("blob: an endpoint is required")
	case opts.Bucket == "":
		return nil, errors.New("blob: a bucket is required")
	case opts.AccessKey == "" || opts.SecretKey == "":
		return nil, errors.New("blob: an access key and a secret key are required")
	}

	if _, err := url.Parse(opts.Endpoint); err != nil {
		return nil, fmt.Errorf("blob: %q is not an endpoint: %w", opts.Endpoint, err)
	}

	region := opts.Region
	if region == "" {
		region = "auto"
	}

	client := s3.NewFromConfig(aws.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(opts.AccessKey, opts.SecretKey, ""),
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(opts.Endpoint)
		o.UsePathStyle = opts.PathStyle
	})

	return &S3{client: client, presign: s3.NewPresignClient(client), bucket: opts.Bucket}, nil
}

// PresignPut signs a URL that accepts exactly one object of exactly `size` bytes.
func (s *S3) PresignPut(ctx context.Context, key string, size int64, ttl time.Duration) (Upload, error) {
	if size <= 0 {
		return Upload{}, fmt.Errorf("blob: an upload of %d bytes is not an upload", size)
	}

	request, err := s.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),

		// Signed, so the store rejects a body of any other length. This is the only
		// place a size limit can be enforced: the API never sees the bytes.
		ContentLength: aws.Int64(size),
	}, func(o *s3.PresignOptions) { o.Expires = ttl })

	if err != nil {
		return Upload{}, fmt.Errorf("blob: sign an upload to %s: %w", key, err)
	}

	return Upload{
		URL:       request.URL,
		Method:    request.Method,
		Headers:   request.SignedHeader,
		ExpiresAt: time.Now().Add(ttl),
	}, nil
}

// PresignGet signs a URL that downloads the object as an attachment.
func (s *S3) PresignGet(ctx context.Context, key, downloadName string, ttl time.Duration) (string, error) {
	request, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),

		// Never inline, never the type the uploader claimed. A submitted file is
		// bytes a stranger chose, and a browser that renders it is a browser running
		// their HTML on the object store's origin.
		ResponseContentDisposition: aws.String(contentDisposition(downloadName)),
		ResponseContentType:        aws.String("application/octet-stream"),
	}, func(o *s3.PresignOptions) { o.Expires = ttl })

	if err != nil {
		return "", fmt.Errorf("blob: sign a download of %s: %w", key, err)
	}
	return request.URL, nil
}

// PresignGetImage signs a URL that displays an image inline, with a fixed image
// content type.
//
// Inline is safe where PresignGet's attachment is not, and for one reason: the
// only caller puts the URL in an <img>, which decodes bytes as an image and never
// runs them, and the store answers from an origin that is not the app's. The type
// is the caller's constrained one, never the uploader's claim.
func (s *S3) PresignGetImage(ctx context.Context, key, contentType string, ttl time.Duration) (string, error) {
	request, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket:                     aws.String(s.bucket),
		Key:                        aws.String(key),
		ResponseContentDisposition: aws.String("inline"),
		ResponseContentType:        aws.String(contentType),
	}, func(o *s3.PresignOptions) { o.Expires = ttl })

	if err != nil {
		return "", fmt.Errorf("blob: sign an image view of %s: %w", key, err)
	}
	return request.URL, nil
}

// Head reports what is really at a key.
func (s *S3) Head(ctx context.Context, key string) (Object, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var missing *types.NotFound
		if errors.As(err, &missing) {
			return Object{}, ErrNotFound
		}
		return Object{}, fmt.Errorf("blob: head %s: %w", key, err)
	}

	object := Object{}
	if out.ContentLength != nil {
		object.Size = *out.ContentLength
	}
	if out.ContentType != nil {
		object.ContentType = *out.ContentType
	}
	if out.ETag != nil {
		object.ETag = strings.Trim(*out.ETag, `"`)
	}
	return object, nil
}

// Delete removes an object, and is content for it to be gone already.
func (s *S3) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("blob: delete %s: %w", key, err)
	}
	return nil
}

/*
contentDisposition builds the header that names the downloaded file.

`mime.FormatMediaType` does the escaping, which matters more than it looks: a
filename holding a quote or a newline is a filename that ends the header and
begins another one, and the header it begins is whoever uploaded it choosing
what the browser does next.

It emits `filename*=utf-8”…` for anything outside ASCII, which every browser
written this century reads. It returns the empty string when it cannot format
the value at all — which is its way of saying no — and a name we cannot name
becomes `download`, because a header that will not parse is worse than a file
with a dull name.
*/
func contentDisposition(name string) string {
	if name == "" {
		return `attachment; filename="download"`
	}

	formatted := mime.FormatMediaType("attachment", map[string]string{"filename": name})
	if formatted == "" {
		return `attachment; filename="download"`
	}
	return formatted
}

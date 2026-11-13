package backupbackend

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type Backend struct{ root, runtimeDir string }

func New(root, runtimeDir string) *Backend { return &Backend{root: root, runtimeDir: runtimeDir} }

// CheckEmpty only reads a pre-existing bucket. The slash suffix is deliberate:
// sibling prefixes are not part of this repository's exact subtree.
func (b *Backend) CheckEmpty(ctx context.Context, c Config, k Credentials) error {
	if e := ValidateConfig(c); e != nil {
		return e
	}
	if e := ValidateCredentials(k); e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	roots, e := x509.SystemCertPool()
	if e != nil {
		return errBackend
	}
	if k.CACertificate != "" {
		roots.AppendCertsFromPEM([]byte(k.CACertificate))
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, ResponseHeaderTimeout: 10 * time.Second}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: boundedTransport{tr}, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errBackend }}
	svc := s3.New(s3.Options{Region: c.Region, BaseEndpoint: aws.String(c.Endpoint), UsePathStyle: c.Lookup == "path", Credentials: credentials.NewStaticCredentialsProvider(k.AccessKeyID, k.SecretAccessKey, k.SessionToken), HTTPClient: client, RetryMaxAttempts: 1})
	if _, e = svc.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(c.Bucket)}); e != nil {
		return errBackend
	}
	out, e := svc.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(c.Bucket), Prefix: aws.String(c.Prefix + "/"), MaxKeys: aws.Int32(1)})
	if e != nil || out.KeyCount == nil || out.IsTruncated == nil || len(out.Contents) != 0 || len(out.CommonPrefixes) != 0 || aws.ToBool(out.IsTruncated) || aws.ToInt32(out.KeyCount) != 0 {
		return errBackend
	}
	return nil
}

type boundedTransport struct{ base http.RoundTripper }

func (t boundedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	v, e := t.base.RoundTrip(r)
	if e == nil {
		v.Body = &limitedBody{Reader: io.LimitReader(v.Body, 65537), Closer: v.Body}
	}
	return v, e
}

type limitedBody struct {
	io.Reader
	io.Closer
}

func (b *Backend) Probe(ctx context.Context, c Config, k Credentials) (Identity, error) {
	return b.invoke(ctx, c, k, false)
}

// Init executes one command only. It never preflights, probes, retries or repairs.
// Any failure may leave remote objects; caller must reconcile read-only.
func (b *Backend) Init(ctx context.Context, c Config, k Credentials) (Identity, error) {
	return b.invoke(ctx, c, k, true)
}
func (b *Backend) invoke(ctx context.Context, c Config, k Credentials, init bool) (Identity, error) {
	var zero Identity
	if e := ValidateConfig(c); e != nil {
		return zero, e
	}
	if e := ValidateCredentials(k); e != nil {
		return zero, e
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if !safeDirectory(b.runtimeDir, false) {
		return zero, errTool
	}
	st, err := os.Lstat(b.runtimeDir)
	if err != nil || st.Mode().Perm() != 0700 {
		return zero, errTool
	}
	dir, e := os.MkdirTemp(b.runtimeDir, "restic-")
	if e != nil {
		return zero, errBackend
	}
	defer os.RemoveAll(dir)
	tool, e := b.ensure(ctx, dir)
	if e != nil {
		return zero, errTool
	}
	version, e := run(ctx, tool, []string{"version"}, []string{"HOME=" + dir, "TMPDIR=" + dir}, dir)
	if e != nil || !strings.HasPrefix(string(version), "restic "+Version+" compiled with ") || !strings.HasSuffix(strings.TrimSpace(string(version)), " on linux/"+runtime.GOARCH) {
		return zero, errTool
	}
	files := map[string]string{"repository": "s3:" + c.Endpoint + "/" + c.Bucket + "/" + c.Prefix, "password": k.Password}
	if k.CACertificate != "" {
		files["ca"] = k.CACertificate
	}
	for name, data := range files {
		if e = os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); e != nil {
			return zero, errBackend
		}
	}
	args, env := commandSpec(c, k, dir, init)
	data, e := run(ctx, tool, args, env, dir)
	if e != nil {
		return zero, errBackend
	}
	id, e := parseIdentity(data, init)
	if e != nil {
		return zero, errIdentity
	}
	return id, nil
}
func commandSpec(c Config, k Credentials, dir string, init bool) ([]string, []string) {
	env := []string{"RESTIC_REPOSITORY_FILE=" + filepath.Join(dir, "repository"), "RESTIC_PASSWORD_FILE=" + filepath.Join(dir, "password"), "AWS_ACCESS_KEY_ID=" + k.AccessKeyID, "AWS_SECRET_ACCESS_KEY=" + k.SecretAccessKey, "AWS_SESSION_TOKEN=" + k.SessionToken, "AWS_DEFAULT_REGION=" + c.Region, "AWS_EC2_METADATA_DISABLED=true", "HOME=" + dir, "TMPDIR=" + dir}
	args := []string{"--no-cache"}
	if init {
		args = append(args, "--json")
	} else {
		args = append(args, "--no-lock")
	}
	args = append(args, "-o", "s3.bucket-lookup="+c.Lookup)
	if k.CACertificate != "" {
		args = append(args, "--cacert", filepath.Join(dir, "ca"))
	}
	if init {
		args = append(args, "init", "--repository-version", "2")
	} else {
		args = append(args, "cat", "config")
	}
	return args, env
}
func parseIdentity(data []byte, init bool) (Identity, error) {
	if len(data) > 65536 {
		return Identity{}, errIdentity
	}
	// Explicit fields accept restic's config chunker_polynomial and init location,
	// while rejecting unknown fields, duplicate keys and trailing documents.
	var fields map[string]json.RawMessage
	d := json.NewDecoder(bytes.NewReader(data))
	tok, e := d.Token()
	if e != nil || tok != json.Delim('{') {
		return Identity{}, errIdentity
	}
	fields = make(map[string]json.RawMessage)
	for d.More() {
		t, e := d.Token()
		if e != nil {
			return Identity{}, errIdentity
		}
		s, ok := t.(string)
		if !ok {
			return Identity{}, errIdentity
		}
		if _, ok = fields[s]; ok {
			return Identity{}, errIdentity
		}
		var v json.RawMessage
		if d.Decode(&v) != nil {
			return Identity{}, errIdentity
		}
		fields[s] = v
	}
	if _, e = d.Token(); e != nil {
		return Identity{}, errIdentity
	}
	if d.Decode(new(any)) != io.EOF {
		return Identity{}, errIdentity
	}
	for key := range fields {
		if key != "id" && ((!init && key != "version" && key != "chunker_polynomial") || (init && key != "message_type" && key != "repository")) {
			return Identity{}, errIdentity
		}
	}
	var id Identity
	if json.Unmarshal(fields["id"], &id.ID) != nil {
		return id, errIdentity
	}
	if init {
		var kind string
		if json.Unmarshal(fields["message_type"], &kind) != nil || kind != "initialized" {
			return id, errIdentity
		}
		id.Version = 2
	} else {
		if json.Unmarshal(fields["version"], &id.Version) != nil {
			return id, errIdentity
		}
	}
	return id, ValidateIdentity(id)
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	cancel   context.CancelFunc
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := 65536 - b.buffer.Len()
	if n > left {
		b.overflow = true
		b.cancel()
		p = p[:left]
	}
	b.buffer.Write(p)
	return n, nil
}
func run(ctx context.Context, path string, args, env []string, dir string) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out, diag := &boundedBuffer{cancel: cancel}, &boundedBuffer{cancel: cancel}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.Stdout = out
	cmd.Stderr = diag
	cmd.WaitDelay = time.Second
	configureProcess(cmd)
	if e := cmd.Run(); e != nil || out.overflow || diag.overflow || ctx.Err() != nil {
		return nil, errBackend
	}
	return out.buffer.Bytes(), nil
}

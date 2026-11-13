package backupbackend

import (
	"context"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func config() Config {
	return Config{Endpoint: "https://storage.example", Bucket: "backups", Prefix: "repositories/operation", Region: "us-east-1", Lookup: "path"}
}
func creds() Credentials {
	return Credentials{AccessKeyID: "synthetic", SecretAccessKey: "synthetic-secret", Password: "synthetic-password"}
}
func TestValidation(t *testing.T) {
	if ValidateConfig(config()) != nil || ValidateCredentials(creds()) != nil {
		t.Fatal("valid input")
	}
	for _, v := range []string{"http://host", "https://user@host", "https://host/", "https://host?", "https://host#fragment", "https://host/a", "https://host\\x"} {
		c := config()
		c.Endpoint = v
		if ValidateConfig(c) == nil {
			t.Fatal(v)
		}
	}
	for _, v := range []string{"", "/a", "a/", "a//b", "a/../b", "a/./b", "a%2fb", strings.Repeat("a", 513)} {
		c := config()
		c.Prefix = v
		if ValidateConfig(c) == nil {
			t.Fatal(v)
		}
	}
	for _, v := range []string{"bad\nvalue", "bad\x00value", strings.Repeat("a", 4097), ""} {
		k := creds()
		k.SecretAccessKey = v
		if ValidateCredentials(k) == nil {
			t.Fatal("credential accepted")
		}
	}
	k := creds()
	k.CACertificate = "not PEM"
	if ValidateCredentials(k) == nil {
		t.Fatal("CA")
	}
	for _, id := range []Identity{{"123", 2}, {strings.Repeat("z", 64), 2}, {strings.Repeat("a", 64), 1}} {
		if ValidateIdentity(id) == nil {
			t.Fatal(id)
		}
	}
}
func TestIdentityOutput(t *testing.T) {
	id := strings.Repeat("a", 64)
	for _, init := range []bool{false, true} {
		data := fmt.Sprintf(`{"version":2,"id":%q,"chunker_polynomial":"abc"}`, id)
		if init {
			data = fmt.Sprintf(`{"message_type":"initialized","id":%q,"repository":"sensitive"}`, id)
		}
		got, e := parseIdentity([]byte(data), init)
		if e != nil || got.ID != id {
			t.Fatal(got, e)
		}
		for _, bad := range []string{data + `{}`, strings.Replace(data, `"id":`, `"id":"duplicate","id":`, 1), `{}`, strings.Replace(data, id, "short", 1), strings.Replace(data, `}`, `,"unexpected":true}`, 1)} {
			if _, e = parseIdentity([]byte(bad), init); e == nil {
				t.Fatal(bad)
			}
		}
	}
}
func TestCheckEmptyTLS(t *testing.T) {
	for _, scenario := range []string{"empty", "object", "truncated", "missing", "denied", "malformed"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method == "HEAD" {
					if scenario == "missing" {
						w.WriteHeader(404)
					}
					return
				}
				if r.Method != "GET" || r.URL.Path != "/backups" || r.URL.Query().Get("prefix") != "repositories/operation/" || r.URL.Query().Get("max-keys") != "1" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(400)
					return
				}
				if !strings.Contains(r.Header.Get("Authorization"), "Credential=synthetic/") {
					t.Error("missing static credentials")
				}
				switch scenario {
				case "denied":
					w.WriteHeader(403)
					fmt.Fprint(w, "SECRET raw failure")
				case "malformed":
					fmt.Fprint(w, "not XML")
				case "object":
					fmt.Fprint(w, `<ListBucketResult><KeyCount>1</KeyCount><Contents><Key>repositories/operation/file</Key></Contents></ListBucketResult>`)
				case "truncated":
					fmt.Fprint(w, `<ListBucketResult><IsTruncated>true</IsTruncated></ListBucketResult>`)
				default:
					fmt.Fprint(w, `<ListBucketResult><KeyCount>0</KeyCount><IsTruncated>false</IsTruncated></ListBucketResult>`)
				}
			}))
			defer server.Close()
			c, k := config(), creds()
			c.Endpoint = server.URL
			k.CACertificate = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
			e := New("/", "/run").CheckEmpty(context.Background(), c, k)
			if (e == nil) != (scenario == "empty") {
				t.Fatal(e)
			}
			if e != nil && strings.Contains(e.Error(), "SECRET") {
				t.Fatal("raw error")
			}
			if scenario == "missing" && calls != 1 {
				t.Fatal(calls)
			}
		})
	}
}
func TestProcessHelper(t *testing.T) {
	if os.Getenv("BACKEND_HELPER") != "1" {
		return
	}
	switch os.Getenv("MODE") {
	case "environment":
		if os.Getenv("AWS_SECRET_ACCESS_KEY") != "" || os.Getenv("RESTIC_PASSWORD_COMMAND") != "" {
			os.Exit(2)
		}
		fmt.Print("clean")
	case "hang":
		time.Sleep(time.Minute)
	case "flood":
		fmt.Print(strings.Repeat("secret", 20000))
	default:
		fmt.Fprint(os.Stderr, "SECRET https://sensitive")
		os.Exit(1)
	}
	os.Exit(0)
}
func TestProcessBoundary(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "inherited-secret")
	t.Setenv("RESTIC_PASSWORD_COMMAND", "arbitrary")
	for _, mode := range []string{"environment", "hang", "flood", "failure"} {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		data, e := run(ctx, os.Args[0], []string{"-test.run=^TestProcessHelper$"}, []string{"BACKEND_HELPER=1", "MODE=" + mode, "GORACE=atexit_sleep_ms=0"}, t.TempDir())
		cancel()
		if mode == "environment" {
			if e != nil || string(data) != "clean" {
				t.Fatal(e, string(data))
			}
		} else if e == nil || strings.Contains(e.Error(), "SECRET") {
			t.Fatal(e)
		}
	}
}
func TestPinsAndUnsafePaths(t *testing.T) {
	a := Artifacts()
	if len(a) != 2 || Version != "0.19.1" {
		t.Fatal(a)
	}
	a[0].SHA256 = "changed"
	if Artifacts()[0].SHA256 == "changed" {
		t.Fatal("mutable")
	}
	p := t.TempDir() + "/binary"
	if os.WriteFile(p, []byte("drift"), 0755) != nil {
		t.Fatal("write")
	}
	if verifyFile(p, Artifacts()[0].SHA256) {
		t.Fatal("drift accepted")
	}
	if runtime.GOOS != "windows" {
		link := p + "-link"
		if os.Symlink(p, link) != nil {
			t.Fatal("symlink")
		}
		if verifyFile(link, Artifacts()[0].SHA256) || safeDirectory(link, false) {
			t.Fatal("symlink accepted")
		}
	}
	b := New(t.TempDir(), t.TempDir())
	if _, e := b.ensure(context.Background(), t.TempDir()); e == nil {
		t.Fatal("unsafe or unsupported tool install")
	}
}

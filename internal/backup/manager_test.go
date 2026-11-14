package backup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nchapman/soar/internal/backupbackend"
)

type fakeBackend struct {
	identity                    backupbackend.Identity
	initErr, probeErr, emptyErr error
	inits, probes, checks       int
	beforeInit                  func()
}

func (f *fakeBackend) Probe(context.Context, backupbackend.Config, backupbackend.Credentials) (backupbackend.Identity, error) {
	f.probes++
	return f.identity, f.probeErr
}
func (f *fakeBackend) CheckEmpty(context.Context, backupbackend.Config, backupbackend.Credentials) error {
	f.checks++
	return f.emptyErr
}
func (f *fakeBackend) Init(context.Context, backupbackend.Config, backupbackend.Credentials) (backupbackend.Identity, error) {
	f.inits++
	if f.beforeInit != nil {
		f.beforeInit()
	}
	return f.identity, f.initErr
}

func setupRepository(t *testing.T) (*Manager, *fakeBackend, Request, []byte, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(root, "var/lib/soar"), 0755); err != nil {
		t.Fatal(err)
	}
	f := &fakeBackend{identity: backupbackend.Identity{ID: strings.Repeat("a", 64), Version: 2}}
	m := &Manager{Root: root, Backend: f, Now: func() time.Time { return time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC) }}
	op := strings.Repeat("b", 32)
	r := Request{Action: "create", Name: "offsite", Operation: op, Confirm: true, Config: &backupbackend.Config{Endpoint: "https://storage.example.com", Bucket: "soar-backups", Prefix: "soar/" + op, Region: "us-east-1", Lookup: "path"}}
	data, err := json.Marshal(backupbackend.Credentials{AccessKeyID: "EXAMPLEACCESSKEY", SecretAccessKey: "private-storage-key", Password: strings.Repeat("c", 64)})
	if err != nil {
		t.Fatal(err)
	}
	return m, f, r, data, strings.Repeat("d", 32)
}

func TestCreatePersistsIntentBeforeSingleInitialization(t *testing.T) {
	m, f, r, data, host := setupRepository(t)
	f.beforeInit = func() {
		saved, err := m.storage().load(r.Name, host)
		if err != nil || !saved.InitAttempted || saved.Metadata.State != "initializing" || saved.Credentials.Password != strings.Repeat("c", 64) {
			t.Fatalf("missing durable private intent: %v", err)
		}
	}
	response, err := m.Execute(context.Background(), host, r, data)
	if err != nil || response.Repository.State != "ready" || response.Repository.RepositoryID != f.identity.ID {
		t.Fatalf("create: %+v %v", response, err)
	}
	if f.inits != 1 || f.checks != 1 || f.probes != 1 {
		t.Fatal("unexpected backend calls")
	}
	if _, err = m.Execute(context.Background(), host, r, data); err != nil {
		t.Fatal(err)
	}
	if f.inits != 1 || f.checks != 1 {
		t.Fatal("idempotent setup repeated initialization")
	}
}

func TestLostInitializationResultOnlyResumesObservation(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed", true: "partial-or-missing"}[missing], func(t *testing.T) {
			m, f, r, data, host := setupRepository(t)
			f.initErr = errors.New("private raw initialization failure")
			if _, err := m.Execute(context.Background(), host, r, data); !errors.Is(err, errUnknown) {
				t.Fatal(err)
			}
			if missing {
				f.probeErr = errors.New("repository missing")
			}
			response, err := m.Execute(context.Background(), host, Request{Action: "resume", Name: r.Name, Operation: r.Operation}, nil)
			if missing && !errors.Is(err, errUnknown) {
				t.Fatalf("partial init: %v", err)
			}
			if !missing && (err != nil || response.Repository.State != "ready") {
				t.Fatalf("reconcile: %v", err)
			}
			if f.inits != 1 || f.checks != 1 {
				t.Fatal("unknown init was repeated")
			}
		})
	}
}

func TestKnownInitializationIDCannotBeReplaced(t *testing.T) {
	m, f, r, data, host := setupRepository(t)
	f.probeErr = errors.New("unavailable")
	if _, err := m.Execute(context.Background(), host, r, data); !errors.Is(err, errVerify) {
		t.Fatal(err)
	}
	f.probeErr = nil
	f.identity.ID = strings.Repeat("e", 64)
	if _, err := m.Execute(context.Background(), host, Request{Action: "resume", Name: r.Name, Operation: r.Operation}, nil); !errors.Is(err, errVerify) {
		t.Fatal("replacement identity was accepted", err)
	}
	saved, err := m.storage().load(r.Name, host)
	if err != nil || saved.Metadata.RepositoryID != strings.Repeat("a", 64) || saved.Metadata.State != "unverified" {
		t.Fatal("pin changed", err)
	}
	if f.inits != 1 {
		t.Fatal("reinitialized")
	}
}

func TestPreflightFailureNeverInitializesAndCanResumeCapturedInputs(t *testing.T) {
	m, f, r, data, host := setupRepository(t)
	f.emptyErr = errors.New("S3 permission denied")
	if _, err := m.Execute(context.Background(), host, r, data); !errors.Is(err, errPreflight) {
		t.Fatal(err)
	}
	if f.inits != 0 {
		t.Fatal("init before preflight")
	}
	f.emptyErr = nil
	if _, err := m.Execute(context.Background(), host, Request{Action: "resume", Name: r.Name, Operation: r.Operation}, nil); err != nil {
		t.Fatal(err)
	}
	if f.inits != 1 {
		t.Fatal("resume did not honor captured approved intent")
	}
}

func TestConnectAndProbeNeverInitialize(t *testing.T) {
	for _, failure := range []string{"", "authentication", "identity"} {
		t.Run(failure, func(t *testing.T) {
			m, f, r, data, host := setupRepository(t)
			r.Action = "connect"
			r.ExpectedID = f.identity.ID
			if failure == "authentication" {
				f.probeErr = errors.New("secret-bearing error")
			}
			if failure == "identity" {
				f.identity.ID = strings.Repeat("f", 64)
			}
			_, err := m.Execute(context.Background(), host, r, data)
			if (err != nil) != (failure != "") {
				t.Fatal("unexpected connect result", err)
			}
			if f.inits != 0 || f.checks != 0 {
				t.Fatal("connect attempted writes")
			}
		})
	}
	m, f, r, data, host := setupRepository(t)
	r.Action = "probe"
	r.Confirm = false
	r.Operation = ""
	response, err := m.Execute(context.Background(), host, r, data)
	if err != nil || response.Repository.RepositoryID != f.identity.ID {
		t.Fatal(err)
	}
	entries, err := m.storage().list(host)
	if err != nil || len(entries) != 0 || f.inits != 0 {
		t.Fatal("probe registered or initialized", err)
	}
}

func TestObservationAndErrorsNeverExposeCredentials(t *testing.T) {
	m, f, r, data, host := setupRepository(t)
	if _, err := m.Execute(context.Background(), host, r, data); err != nil {
		t.Fatal(err)
	}
	for _, request := range []Request{{Action: "list"}, {Action: "status", Name: r.Name}, {Action: "verify", Name: r.Name}} {
		result, err := m.Execute(context.Background(), host, request, nil)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(result)
		for _, secret := range []string{"EXAMPLEACCESSKEY", "private-storage-key", strings.Repeat("c", 64)} {
			if strings.Contains(string(encoded), secret) {
				t.Fatal("metadata leaked credentials")
			}
		}
	}
	f.probeErr = errors.New("private-storage-key")
	_, err := m.Execute(context.Background(), host, Request{Action: "verify", Name: r.Name}, nil)
	if err == nil || strings.Contains(err.Error(), "private-storage-key") || ErrorCode(err) != "backup_verify" {
		t.Fatal("unsafe diagnostic", err)
	}
}

func TestConflictForeignHostAndInputFailBeforeBackend(t *testing.T) {
	m, f, r, data, host := setupRepository(t)
	if _, err := m.Execute(context.Background(), host, r, data); err != nil {
		t.Fatal(err)
	}
	before := f.probes
	changed := r
	changed.Operation = strings.Repeat("e", 32)
	copyConfig := *r.Config
	copyConfig.Prefix = "soar/" + changed.Operation
	changed.Config = &copyConfig
	if _, err := m.Execute(context.Background(), host, changed, data); !errors.Is(err, errConflict) {
		t.Fatal(err)
	}
	if _, err := m.Execute(context.Background(), strings.Repeat("f", 32), Request{Action: "status", Name: r.Name}, nil); !errors.Is(err, errStore) {
		t.Fatal(err)
	}
	if _, err := m.Execute(context.Background(), host, Request{Action: "resume", Name: r.Name, Operation: changed.Operation}, nil); !errors.Is(err, errConflict) {
		t.Fatal(err)
	}
	if _, err := m.Execute(context.Background(), host, Request{Action: "verify", Name: r.Name}, data); !errors.Is(err, errInput) {
		t.Fatal(err)
	}
	if f.probes != before {
		t.Fatal("invalid request reached backend")
	}
}

func TestMetadataEmptyStoreDoesNotCreateState(t *testing.T) {
	m, _, _, _, host := setupRepository(t)
	response, err := m.Execute(context.Background(), host, Request{Action: "list"}, nil)
	if err != nil || response.Repositories == nil || len(response.Repositories) != 0 {
		t.Fatal("empty list", err)
	}
	if _, err = os.Stat(m.storage().dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read created state")
	}
}

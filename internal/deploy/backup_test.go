package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"github.com/nchapman/soar/internal/backup"
	"github.com/nchapman/soar/internal/backupbackend"
	"github.com/nchapman/soar/internal/lifecycle"
	"github.com/nchapman/soar/internal/process"
)

type repositoryBackend struct{ calls int }

func (b *repositoryBackend) Probe(context.Context, backupbackend.Config, backupbackend.Credentials) (backupbackend.Identity, error) {
	b.calls++
	return backupbackend.Identity{ID: strings.Repeat("a", 64), Version: 2}, nil
}
func (b *repositoryBackend) Init(c context.Context, x backupbackend.Config, y backupbackend.Credentials) (backupbackend.Identity, error) {
	return b.Probe(c, x, y)
}
func (b *repositoryBackend) CheckEmpty(context.Context, backupbackend.Config, backupbackend.Credentials) error {
	b.calls++
	return nil
}

func repositoryRequest(host lifecycle.Config) Request {
	op := strings.Repeat("b", 32)
	return Request{Version: 1, Action: "backup-repository", Host: host, Backup: &backup.Request{Action: "create", Name: "offsite", Operation: op, Confirm: true, Config: &backupbackend.Config{Endpoint: "https://s3.example.com", Bucket: "backups", Prefix: "soar/" + op, Region: "us-east-1", Lookup: "path"}}}
}
func repositoryCredentials() []byte {
	data, _ := json.Marshal(backupbackend.Credentials{AccessKeyID: "test-access-key", SecretAccessKey: "private-repository-credential", Password: strings.Repeat("c", 64)})
	return data
}

func TestRepositorySetupIsHostOnlyAndIndependentOfApplications(t *testing.T) {
	m, app := setup(t)
	backend := &repositoryBackend{}
	m.RepositoryBackend = backend
	m.Run = func(context.Context, string, []string, []byte) (process.Result, error) {
		t.Fatal("repository setup ran application commands")
		return process.Result{}, nil
	}
	request := repositoryRequest(app.Host)
	response, err := m.Execute(context.Background(), request, repositoryCredentials())
	if err != nil || response.Backup == nil || response.Backup.Repository.State != "ready" {
		t.Fatalf("setup: %v %+v", err, response)
	}
	if _, err = os.Stat(m.dir(app.App)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("repository setup created application state")
	}
	if backend.calls != 3 {
		t.Fatal("backend not reached")
	}
	request.Backup = &backup.Request{Action: "list"}
	response, err = m.Execute(context.Background(), request, nil)
	if err != nil || len(response.Backup.Repositories) != 1 {
		t.Fatal("metadata listing", err)
	}
	encoded, _ := json.Marshal(response)
	if strings.Contains(string(encoded), "private-repository-credential") {
		t.Fatal("credential leaked")
	}
}

func TestRepositoryRequestsRejectApplicationSelectionAndMixedFields(t *testing.T) {
	_, app := setup(t)
	for _, change := range []func(*Request){
		func(r *Request) { r.App = "hello" }, func(r *Request) { r.Operation = strings.Repeat("b", 32) }, func(r *Request) { r.Config = app.Config },
		func(r *Request) { r.HostIdentity = strings.Repeat("a", 32) }, func(r *Request) { r.Service = "web" }, func(r *Request) { r.Apply = true },
		func(r *Request) { r.Partial = true }, func(r *Request) { r.SecretName = "token" }, func(r *Request) { r.Command = []string{"echo"} },
		func(r *Request) { r.Version = 2 }, func(r *Request) { r.Backup = nil }, func(r *Request) { r.Action = "status" },
	} {
		r := repositoryRequest(app.Host)
		change(&r)
		if ValidateRequest(r) == nil {
			t.Fatal("mixed request accepted")
		}
	}
}

func TestRepositoryWritesUseExistingGlobalHostLock(t *testing.T) {
	m, app := setup(t)
	backend := &repositoryBackend{}
	m.RepositoryBackend = backend
	if err := os.MkdirAll(filepath.Dir(m.path(lifecycle.LockPath)), 0755); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(m.path(lifecycle.LockPath))
	ok, err := lock.TryLock()
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer lock.Unlock()
	r := repositoryRequest(app.Host)
	if _, err = m.Execute(context.Background(), r, repositoryCredentials()); !errors.Is(err, errHostBusy) {
		t.Fatal("mutating request bypassed lock", err)
	}
	if backend.calls != 0 {
		t.Fatal("backend called while lock held")
	}
	for _, action := range []string{"list", "status", "verify", "resume", "probe", "connect", "create"} {
		r.Backup.Action = action
		if r.Mutating() != (action != "list" && action != "status") {
			t.Fatal("incorrect worker/lock classification", action)
		}
	}
}

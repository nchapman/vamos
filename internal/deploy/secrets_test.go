package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/process"
)

const secretCanary = "not-a-real-credential: $ # = \\\" café\t "

func stageSecret(t *testing.T, m *Manager, r Request, id, value string) {
	t.Helper()
	q := r
	q.Action = "secrets-stage"
	q.Config = nil
	q.Supersede = ""
	q.SecretName = "token"
	q.Operation = strings.Repeat(id, 32)
	response, err := m.Execute(context.Background(), q, []byte(value))
	if err != nil {
		t.Fatal(err)
	}
	if response.Secret == nil || response.Secret.Version != q.Operation {
		t.Fatal("missing stage receipt")
	}
}
func secretMapping(r *Request) {
	s := r.Config.Services["web"]
	s.Secrets = map[string]string{"TOKEN": "token"}
	r.Config.Services["web"] = s
}
func secretStatus(t *testing.T, m *Manager, r Request) Response {
	t.Helper()
	r.Action = "status"
	r.Config = nil
	r.Operation = ""
	r.Supersede = ""
	response, err := m.Execute(context.Background(), r, nil)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestRuntimeDockerDiagnosticsDoNotExposeValues(t *testing.T) {
	m, _ := setup(t)
	m.Run = func(context.Context, string, []string, []byte) (process.Result, error) {
		return process.Result{Stderr: []byte(secretCanary)}, errors.New(secretCanary)
	}
	_, err := m.cmd(context.Background(), "docker", "container", "inspect", "owned")
	if err == nil || strings.Contains(err.Error(), secretCanary) {
		t.Fatal("runtime Docker error exposed input")
	}
	m.Run = func(context.Context, string, []string, []byte) (process.Result, error) {
		return process.Result{Stdout: []byte(`[{"Config":{"Env":[{"` + secretCanary + `":true}]}}]`)}, nil
	}
	if _, err = m.inspect(context.Background(), "container", "owned"); err == nil || strings.Contains(err.Error(), secretCanary) {
		t.Fatal("inspection parser exposed input")
	}
}

func TestSecretOnlyIdentityAndMissingPreflight(t *testing.T) {
	m, r := setup(t)
	m.Run = func(context.Context, string, []string, []byte) (process.Result, error) {
		t.Fatal("staging touched Docker")
		return process.Result{}, nil
	}
	stageSecret(t, m, r, "1", secretCanary)
	if _, err := os.Stat(m.dir(r.App)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("staging created app journal directory")
	}
	response := secretStatus(t, m, r)
	if response.Status.Phase != "staged" || response.Status.Running || len(response.Status.Secrets) != 1 || len(response.Status.Secrets[0].Services) != 0 {
		t.Fatal("bad secret-only status")
	}
	r.Action = "secrets-list"
	r.Config = nil
	r.Operation = ""
	response, err := m.Execute(context.Background(), r, nil)
	if err != nil || len(response.SecretVersions) != 1 {
		t.Fatal("missing metadata listing", err)
	}
	b, _ := json.Marshal(response)
	if strings.Contains(string(b), secretCanary) {
		t.Fatal("response contains secret")
	}
	for _, action := range []string{"cleanup", "destroy"} {
		r.Action = action
		r.Operation = strings.Repeat("2", 32)
		r.Apply = true
		response, err = m.Execute(context.Background(), r, nil)
		if err != nil || response.Status.Phase != "staged" {
			t.Fatal("secret-only maintenance", err)
		}
	}
	r.Action = "deploy"
	r.Operation = strings.Repeat("c", 32)
	r.Apply = false
	r.Config = app()
	s := r.Config.Services["web"]
	s.Secrets = map[string]string{"OTHER": "missing"}
	r.Config.Services["web"] = s
	if _, err = m.Execute(context.Background(), r, []byte("archive")); err == nil {
		t.Fatal("missing secret accepted")
	}
	if _, err = os.Stat(m.dir(r.App)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing secret persisted deployment inputs")
	}
}

func TestSecretRotationPinsResumeAndRetainedRelease(t *testing.T) {
	f, r := flowSetup(t)
	legacy := f.execute(r, "legacy", false).Current
	stageSecret(t, f.m, r, "1", secretCanary)
	secretMapping(&r)
	r.Operation = strings.Repeat("e", 32)
	f.runError = true
	f.m.Run = func(c context.Context, n string, args []string, b []byte) (process.Result, error) {
		result, err := f.run(c, n, args, b)
		if n == "docker" && args[0] == "run" {
			return process.Result{Stderr: []byte(secretCanary)}, errors.New(secretCanary)
		}
		return result, err
	}
	j := f.execute(r, "captured", true)
	if strings.Contains(j.LastError, secretCanary) {
		t.Fatal("journal diagnostic contains secret")
	}
	if j.Current.Container != legacy.Container || j.SecretRefs["TOKEN"].Version != strings.Repeat("1", 32) {
		t.Fatal("failed deploy lost captured refs")
	}
	stageSecret(t, f.m, r, "2", "rotated-value")
	f.runError = false
	f.m.Run = f.run
	j = f.execute(flowResume(r), "", false)
	if j.Current.SecretRefs["TOKEN"].Version != strings.Repeat("1", 32) {
		t.Fatal("resume repinned staged values")
	}
	response := secretStatus(t, f.m, r)
	if len(response.Status.Secrets) != 1 || !response.Status.Secrets[0].Pending {
		t.Fatal("rotation not pending")
	}
	r.Operation = strings.Repeat("f", 32)
	j = f.execute(r, "next", false)
	if j.Current.SecretRefs["TOKEN"].Version != strings.Repeat("2", 32) || j.Previous.SecretRefs["TOKEN"].Version != strings.Repeat("1", 32) {
		t.Fatal("release refs not retained")
	}
	if secretStatus(t, f.m, r).Status.Secrets[0].Pending {
		t.Fatal("applied rotation still pending")
	}
	for _, entry := range f.events {
		if strings.Contains(entry, secretCanary) || strings.Contains(entry, "rotated-value") {
			t.Fatal("secret in process argv")
		}
	}
	for _, archive := range f.archives {
		if strings.Contains(archive, secretCanary) {
			t.Fatal("secret in build context")
		}
	}
	entries, err := os.ReadDir(f.m.path("/run"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".soar-env-") {
			t.Fatal("runtime secret file retained")
		}
	}
	err = filepath.WalkDir(f.m.dir(r.App), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		if strings.Contains(string(b), secretCanary) || strings.Contains(string(b), "rotated-value") {
			t.Fatal("secret in release metadata")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMissingSecretsDoNotSupersedeCandidate(t *testing.T) {
	f, r := flowSetup(t)
	f.execute(r, "legacy", false)
	r.Operation = strings.Repeat("d", 32)
	f.unhealthy = true
	before := f.execute(r, "pending", true)
	r.Supersede = r.Operation
	r.Operation = strings.Repeat("e", 32)
	secretMapping(&r)
	f.events = nil
	if _, err := f.m.Execute(context.Background(), r, []byte("replacement")); err == nil {
		t.Fatal("missing secret accepted")
	}
	after, err := f.m.load(r.App)
	if err != nil {
		t.Fatal(err)
	}
	if after.Operation != before.Operation || len(f.events) != 0 {
		t.Fatal("secret preflight mutated pending deployment")
	}
}

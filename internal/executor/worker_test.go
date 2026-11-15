package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/deploy"
	"github.com/nchapman/soar/internal/lifecycle"
	"github.com/nchapman/soar/internal/secrets"
)

func TestFormatAppResponseRedactsSecretsAndPreservesFailureStatus(t *testing.T) {
	secretVal := "SUPER_SECRET_VALUE_NEVER_LEAK"
	secretReq := deploy.Request{Version: 1, Action: "secrets-set", App: "billing"}
	redacted := FormatAppResponse(secretReq, deploy.Response{}, errors.Join(errors.New(secretVal), secrets.ErrOperationConflict))
	if strings.Contains(redacted.Error, secretVal) || redacted.ErrorCode != deploy.ErrorCode(secrets.ErrOperationConflict) {
		t.Fatalf("secret refusal leaked or lost code: %+v", redacted)
	}

	deployReq := deploy.Request{Version: 1, Action: "deploy", App: "billing"}
	exitCode := 23
	partial := deploy.Response{
		Version: 1,
		Status: &deploy.Status{
			App:     "billing",
			Phase:   "releasing",
			Release: &deploy.RunResult{ExitCode: &exitCode},
		},
	}
	formatted := FormatAppResponse(deployReq, partial, errors.New("release hook exited with code 23"))
	if formatted.Status == nil || formatted.Status.Release == nil || formatted.Status.Release.ExitCode == nil || *formatted.Status.Release.ExitCode != 23 || !strings.Contains(formatted.Error, "code 23") {
		t.Fatalf("failure lost partial release status: %+v", formatted)
	}
}

func TestCoordinatedLocksIsolatePerAppActivity(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reqA := deploy.Request{Version: 1, Action: "deploy", App: "app-a", Host: lifecycle.Config{Name: "prod", Domain: "apps.example.com"}}
	busy, unlockA, err := acquireCoordinatedMutation(root, reqA, "app-a", true)
	if err != nil || busy {
		t.Fatalf("acquire app-a failed: busy=%v err=%v", busy, err)
	}
	defer unlockA()

	activeA, err := ProbeMutationActive(root, "app-a")
	if err != nil || !activeA {
		t.Fatalf("app-a should report active mutation: active=%v err=%v", activeA, err)
	}
	activeB, err := ProbeMutationActive(root, "app-b")
	if err != nil || activeB {
		t.Fatalf("app-b should remain idle while app-a mutates: active=%v err=%v", activeB, err)
	}

	// Second concurrent mutation on the same app is refused by the kernel lock.
	busyDup, _, err := acquireCoordinatedMutation(root, reqA, "app-a", true)
	if err != nil || !busyDup {
		t.Fatalf("duplicate app-a lock was not refused: busy=%v err=%v", busyDup, err)
	}

	// Concurrent mutation on a different app succeeds independently.
	reqB := deploy.Request{Version: 1, Action: "deploy", App: "app-b", Host: lifecycle.Config{Name: "prod", Domain: "apps.example.com"}}
	busyB, unlockB, err := acquireCoordinatedMutation(root, reqB, "app-b", true)
	if err != nil || busyB {
		t.Fatalf("concurrent app-b lock failed: busy=%v err=%v", busyB, err)
	}
	unlockB()

	unlockIngress, err := acquireIngressLock(context.Background(), root)
	if err != nil {
		t.Fatalf("ingress lock failed: %v", err)
	}
	unlockIngress()
}

func TestWriteAndReadWorkerResponseValidatesPermissionsAndJSON(t *testing.T) {
	dir := filepath.Join(t.TempDir(), strings.Repeat("a", 32))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadWorkerResponse(dir); err != nil || ok {
		t.Fatalf("missing response should report ok=false: ok=%v err=%v", ok, err)
	}
	want := deploy.Response{Version: 1, Status: &deploy.Status{App: "billing", Phase: "ready"}}
	if err := writeWorkerResponse(dir, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadWorkerResponse(dir)
	if err != nil || !ok || got.Status == nil || got.Status.App != "billing" {
		t.Fatalf("worker response roundtrip failed: got=%+v ok=%v err=%v", got, ok, err)
	}
}

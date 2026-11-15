package deploy

import (
	"errors"
	"github.com/nchapman/vamos/internal/process"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBuildDiagnosticSafety(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	stageMultiSecret(t, f, strings.Repeat("b", 32), "retained-sensitive")
	stageMultiSecret(t, f, strings.Repeat("c", 32), "current-sensitive")
	for _, out := range []process.Result{{}, {Stderr: []byte("retained-sensitive current-sensitive")}, {Stderr: []byte(strings.Repeat("x", process.OutputLimit))}} {
		got := f.m.buildDiagnostic(j, out, errors.New("missing executable retained-sensitive current-sensitive"), 1024)
		if strings.Contains(got, "sensitive") || !strings.Contains(got, "missing executable") || !strings.Contains(got, "[REDACTED]") {
			t.Fatal(got)
		}
	}
	for _, limit := range []int{1024, 4096} {
		got := f.m.buildDiagnostic(j, process.Result{Stdout: []byte(strings.Repeat("界", 3000) + " retained-sensitive current-sensitive")}, errors.New("retained-sensitive current-sensitive"), limit)
		if len(got) > limit || !utf8.ValidString(got) || strings.Contains(got, "sensitive") {
			t.Fatal(got)
		}
	}
	got := f.m.buildDiagnostic(j, process.Result{Stderr: []byte("sensitive" + strings.Repeat("z", process.OutputLimit) + "\nsafe ending")}, nil, 1024)
	if strings.Contains(got, "sensitive") || !strings.Contains(got, "safe ending") {
		t.Fatal(got)
	}
	if err := os.Chmod(f.m.secretStore().Dir, 0777); err != nil {
		t.Fatal(err)
	}
	got = f.m.buildDiagnostic(j, process.Result{Stderr: []byte("retained-sensitive")}, nil, 1024)
	if !strings.Contains(got, "withheld") || strings.Contains(got, "sensitive") {
		t.Fatal(got)
	}
}

func TestBuildDiagnosticCauseAndPartialLine(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	m, j := f.m, f.load()
	for _, out := range []process.Result{{}, {Stderr: []byte(strings.Repeat("x", process.OutputLimit))}} {
		got := m.buildDiagnostic(j, out, errors.New("executable missing"), 1024)
		if got != "build failed: executable missing" {
			t.Fatalf("%q", got)
		}
	}
	got := m.buildDiagnostic(j, process.Result{}, nil, 1024)
	if got != "build failed; no retained diagnostic output" {
		t.Fatal(got)
	}
}

func TestBuildDiagnosticMissingStoreRunRefs(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(multiApp(), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	m, j := f.m, f.load()
	got := m.buildDiagnostic(j, process.Result{Stderr: []byte("run-secret")}, errors.New("run-secret"), 1024, true)
	if strings.Contains(got, "run-secret") || !strings.Contains(got, "withheld") {
		t.Fatal(got)
	}
}

package deploy

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/nchapman/vamos/internal/process"
)

func TestManagerLockSeams(t *testing.T) {
	m, deployReq := setup(t)
	m.Run = func(context.Context, string, []string, []byte) (process.Result, error) {
		return process.Result{}, nil
	}

	var lockedApp string
	var lockedMut, unlocked bool
	m.MutationLock = func(_ context.Context, app string, mut bool) (bool, func(), error) {
		lockedApp, lockedMut = app, mut
		return false, func() { unlocked = true }, nil
	}

	req := Request{Version: 1, Action: "status", App: "demo", Host: deployReq.Host}
	if _, err := m.Execute(context.Background(), req, nil); err == nil || !strings.Contains(err.Error(), "unmanaged") {
		t.Fatalf("expected unmanaged error, got %v", err)
	}
	if lockedApp != "demo" || lockedMut || !unlocked {
		t.Fatalf("unexpected MutationLock call: app=%q mut=%t unlocked=%t", lockedApp, lockedMut, unlocked)
	}

	m.MutationLock = func(context.Context, string, bool) (bool, func(), error) {
		return false, nil, errors.New("queue fence active")
	}
	if _, err := m.Execute(context.Background(), req, nil); err == nil || !strings.Contains(err.Error(), "queue fence active") {
		t.Fatalf("expected lock error, got %v", err)
	}

	busyUnlocked := false
	m.MutationLock = func(_ context.Context, app string, mut bool) (bool, func(), error) {
		if app != deployReq.App || !mut {
			t.Fatalf("unexpected busy MutationLock args: app=%q mut=%t", app, mut)
		}
		return true, func() { busyUnlocked = true }, nil
	}
	if _, err := m.Execute(context.Background(), deployReq, []byte("context")); !errors.Is(err, errHostBusy) || !busyUnlocked {
		t.Fatalf("expected errHostBusy and unlock for active mutating lock, got err=%v unlocked=%t", err, busyUnlocked)
	}

	var ingressLocked, ingressUnlocked bool
	m.IngressLock = func(context.Context) (func(), error) {
		ingressLocked = true
		return func() { ingressUnlocked = true }, nil
	}
	m.HTTP = &http.Client{Transport: transport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	})}
	_ = m.verifyRoutes(context.Background(), &journal{
		Version: 2,
		HostID:  strings.Repeat("a", 32),
		Status:  Status{App: "demo", URL: "https://demo.example.com"},
	})
	if !ingressLocked || !ingressUnlocked {
		t.Fatalf("IngressLock was not cleanly acquired/released: locked=%t unlocked=%t", ingressLocked, ingressUnlocked)
	}

	// Nil unlock returned by IngressLock must not panic, and errors propagate
	// before mutating routes or journals.
	m.IngressLock = func(context.Context) (func(), error) {
		return nil, nil
	}
	_ = m.verifyRoutes(context.Background(), &journal{
		Version: 2,
		HostID:  strings.Repeat("a", 32),
		Status:  Status{App: "demo", URL: "https://demo.example.com"},
	})

	lockErr := errors.New("ingress lock unavailable")
	m.IngressLock = func(context.Context) (func(), error) {
		return nil, lockErr
	}
	for name, call := range map[string]func() error{
		"verifyRoutes": func() error {
			return m.verifyRoutes(context.Background(), &journal{Version: 2, HostID: strings.Repeat("a", 32), Status: Status{App: "demo", URL: "https://demo.example.com"}})
		},
		"promote": func() error {
			return m.promote(context.Background(), &journal{Version: 2, HostID: strings.Repeat("a", 32), Status: Status{App: "demo", URL: "https://demo.example.com"}})
		},
		"reconcilePending": func() error {
			return m.reconcilePending(context.Background(), &journal{Version: 2, HostID: strings.Repeat("a", 32), NewCaddy: baseCaddy, Status: Status{App: "demo", URL: "https://demo.example.com"}})
		},
		"verifyUnservedPromotion": func() error {
			return m.verifyUnservedPromotion(context.Background(), &journal{Version: 2, HostID: strings.Repeat("a", 32), NewCaddy: baseCaddy, Status: Status{App: "demo", URL: "https://demo.example.com"}})
		},
	} {
		if got := call(); !errors.Is(got, lockErr) {
			t.Fatalf("%s did not propagate IngressLock error: %v", name, got)
		}
	}
}

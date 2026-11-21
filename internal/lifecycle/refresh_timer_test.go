package lifecycle

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/gofrs/flock"
	"github.com/nchapman/vamos/internal/process"
)

func TestRefreshTimerRejectsForeignUnit(t *testing.T) {
	m, f := newTestManager(t)
	const p = "/etc/systemd/system/vamos-caddy-refresh.timer"
	f.write(p, "foreign")
	if _, err := m.Bootstrap(context.Background(), testConfig); err == nil {
		t.Fatal("adopted foreign timer")
	}
	b, _ := os.ReadFile(m.path(p))
	if string(b) != "foreign" {
		t.Fatal("changed foreign timer")
	}
}

func TestRefreshTimerMigratesLegacyAndResumes(t *testing.T) {
	ctx := context.Background()
	m, f := newTestManager(t)
	s, err := m.Bootstrap(ctx, testConfig)
	if err != nil {
		t.Fatal(err)
	}
	for name := range refreshUnits {
		if err := os.Remove(m.path(systemUnitDir + name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(m.path(refreshEnableLink)); err != nil {
		t.Fatal(err)
	}
	s.RefreshTimer = ""
	if err := m.save(s); err != nil {
		t.Fatal(err)
	}
	// Even byte-identical pre-existing files must not be adopted.
	f.write(systemUnitDir+refreshService, refreshUnitBody(s, refreshService))
	if _, err := m.Upgrade(ctx, testConfig); err == nil {
		t.Fatal("adopted unowned service")
	}
	if err := os.Remove(m.path(systemUnitDir + refreshService)); err != nil {
		t.Fatal(err)
	}
	m.Run = func(ctx context.Context, name string, args []string) (process.Result, error) {
		if name == "systemctl" && args[0] == "enable" && contains(args, refreshTimer) {
			return process.Result{}, errors.New("interrupted")
		}
		return f.run(ctx, name, args)
	}
	if _, err := m.Upgrade(ctx, testConfig); err == nil {
		t.Fatal("migration unexpectedly succeeded")
	}
	s, err = ReadState(m.path(StatePath))
	if err != nil || s.RefreshTimer != "pending" {
		t.Fatalf("receipt: %+v, %v", s, err)
	}
	if err = m.RefreshCaddy(ctx); err == nil {
		t.Fatal("ran during pending migration")
	}
	if err = os.Remove(m.path(systemUnitDir + refreshTimer)); err != nil {
		t.Fatal(err)
	}
	m.Run = f.run
	s, err = m.Upgrade(ctx, testConfig)
	if err != nil || s.RefreshTimer != "installed" {
		t.Fatalf("resume: %+v, %v", s, err)
	}
	f.write(systemUnitDir+refreshService, "changed")
	if _, err = m.Teardown(ctx, testConfig.Name, true); err == nil {
		t.Fatal("purged modified unit")
	}
	f.write(systemUnitDir+refreshService, refreshUnitBody(s, refreshService))
	if _, err = m.Teardown(ctx, testConfig.Name, true); err != nil {
		t.Fatal(err)
	}
	for name := range refreshUnits {
		if _, err = os.Lstat(m.path(systemUnitDir + name)); !os.IsNotExist(err) {
			t.Fatalf("left %s: %v", name, err)
		}
	}
}

func TestRefreshTimerExcludesAppWorkers(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t)
	s, err := m.Bootstrap(ctx, testConfig)
	if err != nil {
		t.Fatal(err)
	}
	l := flock.New(m.path(LockPath))
	defer l.Close()
	ok, err := l.TryRLock()
	if !ok || err != nil {
		t.Fatal(err)
	}
	if err = m.RefreshCaddy(ctx); err == nil {
		t.Fatal("raced shared worker lock")
	}
	if err = l.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err = m.RefreshCaddy(ctx); err != nil {
		t.Fatal(err)
	}
	s.Phase = "installing"
	if err = m.save(s); err != nil {
		t.Fatal(err)
	}
	if err = m.RefreshCaddy(ctx); err == nil {
		t.Fatal("ran on non-ready host")
	}
}

func TestRefreshTimerHonorsSystemWideServiceDefaults(t *testing.T) {
	m, f := newTestManager(t)
	m.Run = func(ctx context.Context, name string, args []string) (process.Result, error) {
		if name == "systemctl" && args[0] == "show" && args[1] == "--property=DropInPaths" && args[len(args)-1] == refreshService {
			return process.Result{Stdout: []byte("/run/systemd/system/service.d/zzz-lxc-service.conf\n")}, nil
		}
		return f.run(ctx, name, args)
	}
	if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshTimerDriftReportedAndBlocksRefresh(t *testing.T) {
	ctx := context.Background()
	m, f := newTestManager(t)
	_, err := m.Bootstrap(ctx, testConfig)
	if err != nil {
		t.Fatal(err)
	}
	m.Run = func(ctx context.Context, name string, args []string) (process.Result, error) {
		if name == "systemctl" && args[0] == "show" && args[1] == "--property=DropInPaths" {
			return process.Result{Stdout: []byte("/run/systemd/transient/override.conf")}, nil
		}
		return f.run(ctx, name, args)
	}
	if err = m.RefreshCaddy(ctx); err == nil {
		t.Fatal("ignored loaded drop-in")
	}
	s, err := ReadState(m.path(StatePath))
	if err != nil || s.LastError == "" {
		t.Fatalf("status: %+v, %v", s, err)
	}
	m.Run = f.run
	if err = m.RefreshCaddy(ctx); err != nil {
		t.Fatal(err)
	}
	s, err = ReadState(m.path(StatePath))
	if err != nil || s.LastError != "" {
		t.Fatalf("status after recovery: %+v, %v", s, err)
	}
}

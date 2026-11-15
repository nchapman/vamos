package lifecycle

import (
	"context"
	"os"
	"strings"
	"testing"
)

func indexOf(calls []string, prefix string) int {
	for i, call := range calls {
		if strings.HasPrefix(call, prefix) {
			return i
		}
	}
	return -1
}

func TestSetupRunsPinnedHardenedCaddyContainer(t *testing.T) {
	m, f := newTestManager(t)
	s, err := m.Bootstrap(context.Background(), testConfig)
	if err != nil {
		t.Fatal(err)
	}
	if f.caddy == nil || f.caddy.Image != CaddyImage || !f.caddy.Running || s.CaddyContainerID != f.caddy.ID {
		t.Fatalf("caddy=%+v state=%+v", f.caddy, s)
	}
	run := strings.Join(f.caddy.Args, " ")
	for _, want := range []string{
		"--network host", "--restart unless-stopped", "--read-only", "--cap-drop ALL", "--cap-add NET_BIND_SERVICE",
		"--security-opt no-new-privileges", "type=bind,source=/etc/caddy,target=/etc/caddy,readonly",
		"type=bind,source=/var/lib/caddy/data,target=/data", caddyHostLabel + "=" + s.ID,
	} {
		if !strings.Contains(run, want) {
			t.Errorf("caddy run missing %q: %s", want, run)
		}
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "apt-get") && strings.Contains(call, "caddy") {
			t.Fatalf("setup installed a distribution Caddy package: %s", call)
		}
	}
	info, err := os.Stat(m.path(caddyDataDir))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("caddy data directory: %v %v", info, err)
	}
	if indexOf(f.calls, "docker run") > indexOf(f.calls, "systemctl enable --now "+agentUnit) {
		t.Fatal("agent started before ingress")
	}
}

func TestUpgradeMovesCaddyToPinnedImageAndKeepsRoutes(t *testing.T) {
	m, f := newTestManager(t)
	if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
		t.Fatal(err)
	}
	old := "caddy:2.10.0@sha256:" + strings.Repeat("0", 64)
	f.caddy.Image = old
	f.caddyImages[digestRef(old)] = true
	oldID := f.caddy.ID
	if _, err := m.Bootstrap(context.Background(), testConfig); err == nil || !strings.Contains(err.Error(), "server upgrade") {
		t.Fatalf("repeat setup accepted an outdated Caddy image: %v", err)
	}
	routes := caddyConfig + "\nbilling.apps.example.com {\n\treverse_proxy 127.0.0.1:12001\n}\n"
	if err := os.WriteFile(m.path(caddyfilePath), []byte(routes), 0644); err != nil {
		t.Fatal(err)
	}
	before := len(f.calls)
	s, err := m.Upgrade(context.Background(), testConfig)
	if err != nil {
		t.Fatal(err)
	}
	if f.caddy.Image != CaddyImage || f.caddy.ID == oldID || s.CaddyContainerID != f.caddy.ID || f.caddyImages[digestRef(old)] {
		t.Fatalf("caddy=%+v state=%s images=%v", f.caddy, s.CaddyContainerID, f.caddyImages)
	}
	calls := f.calls[before:]
	if pull, remove := indexOf(calls, "docker pull"), indexOf(calls, "docker container rm"); pull < 0 || remove < pull {
		t.Fatalf("old ingress removed before the new image was pulled: %v", calls)
	}
	data, err := os.ReadFile(m.path(caddyfilePath))
	if err != nil || string(data) != routes {
		t.Fatal("upgrade changed live routes")
	}
}

func TestUpgradeRepairsStoppedOrMissingCaddy(t *testing.T) {
	for _, kind := range []string{"stopped", "missing"} {
		t.Run(kind, func(t *testing.T) {
			m, f := newTestManager(t)
			if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
				t.Fatal(err)
			}
			if kind == "stopped" {
				f.caddy.Running = false
			} else {
				f.caddy = nil
			}
			if _, err := m.Bootstrap(context.Background(), testConfig); err == nil {
				t.Fatal("repeat setup reported unhealthy ingress as ready")
			}
			s, err := m.Upgrade(context.Background(), testConfig)
			if err != nil {
				t.Fatal(err)
			}
			if f.caddy == nil || !f.caddy.Running || s.CaddyContainerID != f.caddy.ID {
				t.Fatalf("ingress not repaired: %+v", f.caddy)
			}
		})
	}
}

func TestCaddyContainerFromAnotherInstallationIsNeverAdopted(t *testing.T) {
	m, f := newTestManager(t)
	if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
		t.Fatal(err)
	}
	f.caddy.Labels[caddyHostLabel] = strings.Repeat("f", 32)
	foreign := *f.caddy
	if _, err := m.Upgrade(context.Background(), testConfig); err == nil {
		t.Fatal("upgrade adopted a foreign Caddy container")
	}
	if _, err := m.Teardown(context.Background(), testConfig.Name, true); err == nil {
		t.Fatal("teardown removed a foreign Caddy container")
	}
	if f.caddy == nil || f.caddy.ID != foreign.ID || !f.caddy.Running {
		t.Fatal("foreign container was modified")
	}
}

func TestTeardownRemovesCaddyBeforeStoppingDocker(t *testing.T) {
	m, f := newTestManager(t)
	if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
		t.Fatal(err)
	}
	before := len(f.calls)
	if _, err := m.Teardown(context.Background(), testConfig.Name, true); err != nil {
		t.Fatal(err)
	}
	if f.caddy != nil || len(f.caddyImages) != 0 {
		t.Fatalf("caddy resources remain: %+v %v", f.caddy, f.caddyImages)
	}
	calls := f.calls[before:]
	if remove, stop := indexOf(calls, "docker container rm"), indexOf(calls, "systemctl stop docker.service"); remove < 0 || stop < remove {
		t.Fatalf("ingress not removed while Docker was running: %v", calls)
	}
	if _, err := os.Stat(m.path("/var/lib/caddy")); !os.IsNotExist(err) {
		t.Fatal("caddy certificates and state were not purged")
	}
}

func TestDigestRef(t *testing.T) {
	for in, want := range map[string]string{
		"caddy:2.11.9@sha256:abc":               "caddy@sha256:abc",
		"caddy@sha256:abc":                      "caddy@sha256:abc",
		"registry:5000/team/caddy:2@sha256:abc": "registry:5000/team/caddy@sha256:abc",
		"registry:5000/team/caddy@sha256:abc":   "registry:5000/team/caddy@sha256:abc",
		"caddy:2.11.9":                          "",
	} {
		if got := digestRef(in); got != want {
			t.Errorf("digestRef(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTeardownAllowsOnlyTheRunningIngressMounts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		foreign string
	}{{"ingress", ""}, {"other rootfs", "/var/lib/docker/rootfs/overlayfs/" + strings.Repeat("f", 64)}, {"volume named like the container", "/var/lib/docker/volumes/%s/_data"}} {
		t.Run(tc.name, func(t *testing.T) {
			m, f := newTestManager(t)
			if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
				t.Fatal(err)
			}
			m.MountInfo = func() ([]byte, error) {
				var lines []string
				if f.caddy != nil {
					lines = append(lines, "40 25 0:40 / /var/lib/docker/rootfs/overlayfs/"+f.caddy.ID+" rw - overlay overlay rw")
				}
				if tc.foreign != "" {
					lines = append(lines, "41 25 0:41 / "+strings.ReplaceAll(tc.foreign, "%s", f.caddy.ID)+" rw - ext4 /dev/vdb rw")
				}
				return []byte(strings.Join(lines, "\n")), nil
			}
			_, err := m.Teardown(context.Background(), testConfig.Name, true)
			if tc.foreign != "" {
				if err == nil || !strings.Contains(err.Error(), "overlaps owned data") || f.caddy == nil {
					t.Fatalf("foreign Docker mount accepted or ingress removed: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("running ingress blocked teardown: %v", err)
			}
		})
	}
}

func TestTeardownChecksContainerdBeforeRemovingIngress(t *testing.T) {
	for _, resource := range []string{"containers", "images"} {
		t.Run(resource, func(t *testing.T) {
			m, f := newTestManager(t)
			if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
				t.Fatal(err)
			}
			f.foreignCtr = resource
			if _, err := m.Teardown(context.Background(), testConfig.Name, true); err == nil || !strings.Contains(err.Error(), "containerd has "+resource) {
				t.Fatalf("foreign containerd %s accepted: %v", resource, err)
			}
			if f.caddy == nil || !f.caddy.Running {
				t.Fatal("ingress removed before refusal")
			}
		})
	}
}

func TestTeardownRemovesOutdatedIngressImage(t *testing.T) {
	m, f := newTestManager(t)
	if _, err := m.Bootstrap(context.Background(), testConfig); err != nil {
		t.Fatal(err)
	}
	old := "caddy:2.10.0@sha256:" + strings.Repeat("0", 64)
	f.caddy.Image = old
	f.caddyImages[digestRef(old)] = true
	if _, err := m.Teardown(context.Background(), testConfig.Name, true); err != nil {
		t.Fatal(err)
	}
	if f.caddy != nil || len(f.caddyImages) != 0 {
		t.Fatalf("ingress resources remain: %+v %v", f.caddy, f.caddyImages)
	}
}

package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/nchapman/vamos/internal/config"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestPublishedPortsStatusObservation(t *testing.T) {
	for _, mode := range []string{"running", "stopped", "restarting", "missing", "foreign", "drift", "candidate", "candidate drift", "partial replacement"} {
		t.Run(mode, func(t *testing.T) {
			f := newMultiFlow(t)
			if _, err := f.deploy(portsFlowApp(t, "one"), strings.Repeat("1", 32)); err != nil {
				t.Fatal(err)
			}
			j := f.load()
			slot := j.ServiceStates["backend"]
			v := f.docker.containers[slot.Current.Container]
			want := slices.Clone(slot.Current.Config.Ports)
			slices.Sort(want)
			switch mode {
			case "stopped":
				v.State.Running = false
				v.NetworkSettings.Ports = nil
			case "restarting":
				v.State.Restarting = true
				v.NetworkSettings.Ports = nil
			case "missing":
				delete(f.docker.containers, slot.Current.Container)
			case "foreign":
				v.Config.Labels["vamos.host"] = "foreign"
			case "drift":
				v.NetworkSettings.Ports = nil
			case "candidate", "candidate drift":
				slot.Candidate, slot.Current = slot.Current, nil
				if mode == "candidate drift" {
					v.NetworkSettings.Ports = nil
				}
			case "partial replacement":
				next := *slot.Current
				nextConfig := *slot.Current.Config
				nextConfig.Ports = []string{"9999:9999/udp"}
				next.Config = &nextConfig
				slot.Candidate = &next
				requested := j.Config.Services["backend"]
				requested.Ports = nextConfig.Ports
				j.Config.Services["backend"] = requested
			}
			s := f.m.multiStatus(context.Background(), j, nil, false, "backend")
			if len(s.Services) != 1 {
				t.Fatal("service selection ignored")
			}
			current := s.Services["backend"].PublishedPorts
			candidate := s.Services["backend"].CandidatePublishedPorts
			got := current
			if strings.HasPrefix(mode, "candidate") {
				if current != nil {
					t.Fatal("candidate mislabeled current")
				}
				got = candidate
			}
			if got == nil || !reflect.DeepEqual(got.Mappings, want) {
				t.Fatalf("recorded mappings lost: %+v", got)
			}
			observed := mode == "running" || mode == "candidate" || mode == "partial replacement"
			if got.Observed != observed {
				t.Fatalf("observed=%t want=%t (%s)", got.Observed, observed, s.LastError)
			}
			if mode == "partial replacement" && (candidate == nil || candidate.Observed || !reflect.DeepEqual(candidate.Mappings, []string{"9999:9999/udp"})) {
				t.Fatalf("candidate not distinguished: %+v", candidate)
			}
		})
	}
}

func TestDetachedCandidatePortsAreNotObserved(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(portsFlowApp(t, "one"), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	j := f.load()
	slot := j.ServiceStates["web"]
	slot.Candidate, slot.Current = slot.Current, nil
	slot.Phase = "aliasing"
	j.Phase = "updating"
	v := f.docker.containers[slot.Candidate.Container]
	v.NetworkSettings.Networks = nil
	status := f.m.multiStatus(context.Background(), j, nil, false, "web").Services["web"]
	if len(status.CandidateInstances) != 1 || !status.CandidateInstances[0].Running {
		t.Fatal("fixture did not exercise running detached candidate")
	}
	if status.CandidatePublishedPorts == nil || status.CandidatePublishedPorts.Observed {
		t.Fatal("detached candidate reported observed bindings")
	}
}

func TestPublishedPortsStatusBounds(t *testing.T) {
	ports := make([]string, config.MaxPortBindings)
	for i := range ports {
		ports[i] = fmt.Sprintf("%d:%d/udp", 10000+i, 20000+i)
	}
	r := &release{Config: &config.Service{Ports: ports}}
	got := recordedPublishedPorts(r)
	if got.Total != len(ports) || len(got.Mappings) != maxStatusPortMappings || got.Observed {
		t.Fatalf("bad bounded metadata: %+v", got)
	}
	got.Mappings[0] = "changed"
	if ports[0] == "changed" {
		t.Fatal("status aliases journal")
	}
	r.Config.Ports = []string{"49160-49162:49160-49162/udp"}
	got = recordedPublishedPorts(r)
	if got.Total != 1 || !reflect.DeepEqual(got.Mappings, r.Config.Ports) {
		t.Fatal("range expanded")
	}
	if recordedPublishedPorts(nil) != nil || recordedPublishedPorts(&release{}) != nil || recordedPublishedPorts(&release{Config: &config.Service{}}) != nil {
		t.Fatal("invented port mappings")
	}
}

func TestPublishedPortsStatusWire(t *testing.T) {
	f := newMultiFlow(t)
	if _, err := f.deploy(portsFlowApp(t, "one"), strings.Repeat("1", 32)); err != nil {
		t.Fatal(err)
	}
	status := f.m.multiStatus(context.Background(), f.load(), nil, false, "")
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Services map[string]struct {
			Ports *struct {
				Mappings []string `json:"mappings"`
				Total    int      `json:"total"`
				Observed bool     `json:"observed"`
			} `json:"published_ports"`
		} `json:"services"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"web", "backend"} {
		ports := wire.Services[name].Ports
		if ports == nil || !ports.Observed || ports.Total != len(portsFlowApp(t, "one").Services[name].Ports) {
			t.Fatalf("%s missing observed published ports: %s", name, data)
		}
	}
}

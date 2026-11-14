// Package engine provides the shared Docker container, bridge network,
// resource-limit, private env-file, and HTTP readiness primitives used by the
// production deployment control plane and future local development runners.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nchapman/soar/internal/config"
	"github.com/nchapman/soar/internal/process"
	"github.com/nchapman/soar/internal/secrets"
)

type Runner func(context.Context, string, []string, []byte) (process.Result, error)

// Inspection models the Docker inspect JSON document for containers, images,
// networks, and volumes.
type Inspection struct {
	ID           string `json:"Id"`
	Name         string
	RestartCount int
	Driver       string
	Scope        string
	EnableIPv6   bool
	EnableIPv4   *bool
	Ingress      bool
	Options      map[string]string
	Internal     bool
	Labels       map[string]string
	Containers   map[string]json.RawMessage
	Config       struct {
		Env        []string
		Cmd        []string
		Entrypoint []string
		User       string
		WorkingDir string
		Image      string
		Labels     map[string]string
		Volumes    map[string]json.RawMessage
	}
	HostConfig struct {
		NanoCPUs               int64 `json:"NanoCpus"`
		Memory                 int64
		MemorySwap             int64
		MemoryReservation      int64
		CPUPeriod              int64 `json:"CpuPeriod"`
		CPUQuota               int64 `json:"CpuQuota"`
		CPUShares              int64 `json:"CpuShares"`
		CpusetCpus, CpusetMems string
		OomKillDisable         *bool

		NetworkMode                    string
		Privileged                     bool
		PublishAllPorts                bool
		ReadonlyRootfs                 bool
		CapAdd, CapDrop                []string
		Devices                        []json.RawMessage
		DeviceRequests                 []json.RawMessage
		VolumesFrom, Links, ExtraHosts []string
		DNS                            []string `json:"Dns"`
		DNSOptions                     []string `json:"DnsOptions"`
		DNSSearch                      []string `json:"DnsSearch"`
		SecurityOpt                    []string
		PidMode, IpcMode, UTSMode      string
		Mounts                         []json.RawMessage
		Binds                          []string
		RestartPolicy                  struct{ Name string }
		PortBindings                   map[string][]struct{ HostIP, HostPort string }
	}
	State struct {
		Running    bool
		Restarting bool
		Paused     bool
		Dead       bool
		StartedAt  string
	}
	Mounts          []json.RawMessage
	NetworkSettings struct {
		Networks map[string]json.RawMessage
		Ports    map[string][]struct{ HostIP, HostPort string }
	}
}

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Client executes container-engine primitives against Docker via Run and HTTP.
type Client struct {
	Run          func(context.Context, string, ...string) ([]byte, error)
	HTTP         *http.Client
	AllocatePort func() (string, error)
	RuntimeDir   string
}

func AllocateLoopbackPort() (string, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	if err := l.Close(); err != nil {
		return "", err
	}
	return port, nil
}

func (c *Client) Inspect(ctx context.Context, kind, name string) (*Inspection, error) {
	b, err := c.Run(ctx, "docker", kind, "inspect", name)
	if err != nil {
		return nil, err
	}
	var v []Inspection
	if err = json.Unmarshal(b, &v); err != nil {
		return nil, errors.New("invalid Docker inspection response")
	}
	if len(v) != 1 {
		return nil, errors.New("expected one inspected object")
	}
	return &v[0], nil
}

// Exists lists before inspecting so an inspection failure is never mistaken for absence.
func (c *Client) Exists(ctx context.Context, kind, name string) (bool, error) {
	args := []string{kind, "ls", "--format", "{{json .}}"}
	if kind == "container" {
		args = append(args, "--all")
	}
	b, err := c.Run(ctx, "docker", args...)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var row struct{ Name, Names string }
		if err = json.Unmarshal([]byte(line), &row); err != nil {
			return false, err
		}
		if row.Name == "" && row.Names == "" {
			return false, errors.New("Docker inventory row has no resource name")
		}
		if row.Name == name || row.Names == name {
			return true, nil
		}
	}
	return false, nil
}

var defaultBridgeOptions = map[string]string{
	"com.docker.network.bridge.enable_icc":           "true",
	"com.docker.network.bridge.enable_ip_masquerade": "true",
	"com.docker.network.bridge.gateway_mode_ipv4":    "nat",
	"com.docker.network.bridge.gateway_mode_ipv6":    "nat",
	"com.docker.network.bridge.host_binding_ipv4":    "0.0.0.0",
	"com.docker.network.bridge.inhibit_ipv4":         "false",
	"com.docker.network.bridge.enable_ip_forwarding": "true",
}

func ValidateBridgeNetwork(v *Inspection, hostID, app string) error {
	if v == nil || v.Labels["soar.host"] != hostID || v.Labels["soar.app"] != app || v.Driver != "bridge" || v.Internal || v.EnableIPv6 || v.Ingress || (v.EnableIPv4 != nil && !*v.EnableIPv4) || (v.Scope != "" && v.Scope != "local") {
		return errors.New("foreign or unsafe app network")
	}
	for key, value := range v.Options {
		if expected, ok := defaultBridgeOptions[key]; !ok || value != expected {
			return errors.New("unsupported app network options")
		}
	}
	return nil
}

func ResourceArgs(r *config.Resources) []string {
	if r == nil {
		return nil
	}
	var args []string
	if r.CPUs != nil {
		args = append(args, "--cpus", strconv.FormatFloat(*r.CPUs, 'f', -1, 64))
	}
	if r.MemoryMB != nil {
		memory := strconv.FormatInt(r.MemoryBytes(), 10)
		// Docker defines MemorySwap as the total memory+swap budget. Equal values
		// disable additional swap; leaving it unset would permit extra swap.
		args = append(args, "--memory", memory, "--memory-swap", memory)
	}
	return args
}

func (c *Client) RequireResourceSupport(ctx context.Context, r *config.Resources) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("invalid runtime resources: %w", err)
	}
	if r == nil {
		return nil
	}
	b, err := c.Run(ctx, "docker", "info", "--format", "{{json .}}")
	if err != nil {
		return errors.New("inspect Docker resource capabilities failed")
	}
	var info struct {
		MemoryLimit, SwapLimit bool
		NCPU                   int
		CPUCfsQuota            bool `json:"CpuCfsQuota"`
		CPUCfsPeriod           bool `json:"CpuCfsPeriod"`
	}
	if json.Unmarshal(b, &info) != nil {
		return errors.New("invalid Docker resource capabilities response")
	}
	checks := []struct {
		requested, supported bool
		name                 string
	}{
		{r.CPUs != nil, info.CPUCfsQuota, "CpuCfsQuota"},
		{r.CPUs != nil, info.CPUCfsPeriod, "CpuCfsPeriod"},
		{r.MemoryMB != nil, info.MemoryLimit, "MemoryLimit"},
		{r.MemoryMB != nil, info.SwapLimit, "SwapLimit"},
	}
	for _, check := range checks {
		if check.requested && !check.supported {
			return fmt.Errorf("Docker does not support requested resource limits: %s unavailable", check.name)
		}
	}
	if r.CPUs != nil && (info.NCPU < 1 || *r.CPUs > float64(info.NCPU)) {
		return errors.New("requested CPU limit exceeds Docker's available CPUs or CPU count is unavailable")
	}
	return nil
}

func VerifyResources(v *Inspection, r *config.Resources) error {
	if err := r.Validate(); err != nil {
		return errors.New("invalid captured runtime resources")
	}
	h := &v.HostConfig
	if h.NanoCPUs != r.NanoCPUs() || h.Memory != r.MemoryBytes() || h.MemorySwap != r.MemoryBytes() {
		return errors.New("runtime CPU or memory limits differ from captured configuration")
	}
	if h.CPUPeriod != 0 || h.CPUQuota != 0 || h.CPUShares != 0 || h.CpusetCpus != "" || h.CpusetMems != "" || h.MemoryReservation != 0 || (h.OomKillDisable != nil && *h.OomKillDisable) {
		return errors.New("unexpected competing runtime CPU or memory settings")
	}
	return nil
}

func SafePath(p string) error {
	for {
		s, e := os.Lstat(p)
		if e == nil && s.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink %s", p)
		}
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		q := filepath.Dir(p)
		if q == p {
			return nil
		}
		p = q
	}
}

// RunWithEnvFile writes literal values into a private 0600 temporary file under
// c.RuntimeDir (never argv or Docker's environment), injects --env-file before
// image in args, executes docker, and deletes the temporary file.
func (c *Client) RunWithEnvFile(ctx context.Context, image string, args []string, values map[string]string) error {
	if len(values) == 0 {
		_, err := c.Run(ctx, "docker", args...)
		return err
	}
	dir := c.RuntimeDir
	if dir == "" {
		return errors.New("runtime staging directory is required")
	}
	if err := SafePath(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".soar-env-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var content strings.Builder
	for _, key := range keys {
		if !envKeyPattern.MatchString(key) || secrets.ValidateValue([]byte(values[key])) != nil {
			return errors.New("invalid runtime secret")
		}
		fmt.Fprintf(&content, "%s=%s\n", key, values[key])
	}
	if _, err = f.WriteString(content.String()); err != nil {
		return errors.New("write runtime secret file failed")
	}
	if err = f.Close(); err != nil {
		return errors.New("close runtime secret file failed")
	}
	imageIndex := -1
	for i, arg := range args {
		if arg == image {
			imageIndex = i
			break
		}
	}
	if imageIndex < 0 {
		return errors.New("missing container image argument")
	}
	path, err := filepath.Abs(f.Name())
	if err != nil {
		return err
	}
	command := append([]string{}, args[:imageIndex]...)
	command = append(command, "--env-file", path)
	command = append(command, args[imageIndex:]...)
	_, err = c.Run(ctx, "docker", command...)
	return err
}

func Wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// WaitHTTPReady polls checkPort until it returns a loopback port and the HTTP
// GET to healthPath succeeds with 2xx, or until ctx/timeout expires.
func (c *Client) WaitHTTPReady(ctx context.Context, timeout time.Duration, healthPath string, checkPort func(context.Context) (string, error)) error {
	if c.HTTP == nil {
		return errors.New("HTTP client is required")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		port, err := checkPort(ctx)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:"+port+healthPath, nil)
		if err != nil {
			return fmt.Errorf("invalid readiness URL: %w", err)
		}
		resp, err := c.HTTP.Do(req)
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
		if err = Wait(ctx, 250*time.Millisecond); err != nil {
			return fmt.Errorf("HTTP readiness: %w", err)
		}
	}
}

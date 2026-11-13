package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/nchapman/soar/internal/config"
)

func resourceArgs(r *config.Resources) []string {
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

// Check the daemon, not the client or host configuration: Docker may otherwise
// warn and discard unsupported limits. Nil preserves the old unlimited path.
func (m *Manager) requireResourceSupport(ctx context.Context, r *config.Resources) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("invalid runtime resources: %w", err)
	}
	if r == nil {
		return nil
	}
	b, err := m.cmd(ctx, "docker", "info", "--format", "{{json .}}")
	if err != nil {
		return errors.New("inspect Docker resource capabilities failed")
	}
	// Field names and bool types follow Moby api/types/system.Info.
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

func verifyResources(v *inspection, r *config.Resources) error {
	if err := r.Validate(); err != nil {
		return errors.New("invalid captured runtime resources")
	}
	h := &v.HostConfig
	if h.NanoCPUs != r.NanoCPUs() || h.Memory != r.MemoryBytes() || h.MemorySwap != r.MemoryBytes() {
		return errors.New("runtime CPU or memory limits differ from captured configuration")
	}
	// These settings are never requested by Soar and must not compete with the
	// captured hard caps (including the default unlimited configuration).
	if h.CPUPeriod != 0 || h.CPUQuota != 0 || h.CPUShares != 0 || h.CpusetCpus != "" || h.CpusetMems != "" || h.MemoryReservation != 0 || (h.OomKillDisable != nil && *h.OomKillDisable) {
		return errors.New("unexpected competing runtime CPU or memory settings")
	}
	return nil
}

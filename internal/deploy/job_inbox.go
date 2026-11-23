package deploy

import (
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/nchapman/vamos/internal/jobresults"
)

// History belongs to a service incarnation, not a particular release or job
// declaration. A rollout may remove the schedule without removing its inbox.
func readJobInbox(store jobresults.Store, j *journal, r Request) (Response, error) {
	resp := Response{Version: 1}
	if j == nil {
		return resp, errors.New("app is unmanaged")
	}
	var runs []jobresults.Run
	if isMulti(j) && j.Phase != "destroyed" {
		for service, slot := range j.ServiceStates {
			if (r.Service != "" && r.Service != service) || slot == nil || slot.Current == nil {
				continue
			}
			inc := serviceIncarnation(j, service)
			if r.JobID != "" {
				run, err := store.Get(j.HostID, j.App, service, inc, r.JobID)
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					return resp, err
				}
				resp.JobRun = &run
				return resp, nil
			}
			records, err := store.List(j.HostID, j.App, service, inc)
			if err != nil {
				return resp, err
			}
			for _, run := range records {
				run.Output = nil
				runs = append(runs, run)
			}
		}
	}
	if r.JobID != "" {
		return resp, fmt.Errorf("job run not retained for this selection: %w", os.ErrNotExist)
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].Scheduled.Equal(runs[j].Scheduled) {
			return runs[i].ID > runs[j].ID
		}
		return runs[i].Scheduled.After(runs[j].Scheduled)
	})
	resp.JobRuns = runs[:min(len(runs), jobresults.MaxRuns)]
	return resp, nil
}

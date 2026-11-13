package deploy

import "reflect"

// recordServiceHistory must run only after all services have successfully
// completed, and before Completed is replaced. A partially promoted operation
// is not a completed release: its recovery baseline remains Completed, not the
// mixed Current slots. Unchanged services retain their independent predecessor.
func recordServiceHistory(j *journal) {
	if !supportsServiceIdentity(j) || j.Completed == nil {
		return
	}
	for name, s := range j.ServiceStates {
		if s == nil || s.Current == nil || s.CurrentRetired || s.Unavailable {
			continue
		}
		stable := j.Completed.Services[name]
		if stable != nil && !reflect.DeepEqual(stable, s.Current) {
			s.Previous = stable
		}
	}
}

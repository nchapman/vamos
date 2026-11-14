package backup

import (
	"context"
	"errors"
	"slices"
)

var (
	errPlanInput      = errors.New("invalid backup plan request")
	errPlanConflict   = errors.New("backup plan revision or resolved identities changed; preview again before applying")
	errPlanSelection  = errors.New("backup plan source, credential selector, service identity, or volume binding is unavailable")
	errPlanRepository = errors.New("backup plan requires a locally verified repository with a pinned identity")
)

func (m *Manager) resolvePlan(hostID string, plan Plan, runtime PlanRuntime) (PlanRecord, error) {
	record := PlanRecord{Version: 1, HostID: hostID, Plan: plan, Bindings: PlanBindings{Sources: map[string]SourceBinding{}, Stops: map[string]string{}, Repositories: map[string]RepositoryPin{}}}
	if runtime.HostID != hostID || runtime.App != plan.App || len(runtime.Services) == 0 || len(runtime.Services) > 8 {
		return record, errPlanSelection
	}
	service := func(name string) (PlanService, bool) {
		value, ok := runtime.Services[name]
		return value, ok && ValidOperation(value.Incarnation) && value.Replicas >= 1 && value.Replicas <= 8
	}
	for name, source := range plan.Sources {
		selected, ok := service(source.Service)
		if !ok {
			return record, errPlanSelection
		}
		binding := SourceBinding{Service: source.Service, Incarnation: selected.Incarnation}
		if source.Type == "postgres" {
			credentials, ok := service(source.Credentials.Service)
			if !ok || selected.Replicas != 1 || !slices.Contains(credentials.SecretEnvs, source.Credentials.Env) {
				return record, errPlanSelection
			}
			binding.CredentialService = source.Credentials.Service
			binding.CredentialIncarnation = credentials.Incarnation
		} else {
			if selected.Volume == nil || selected.Volume.Name != source.Volume || !ValidOperation(selected.Volume.ID) {
				return record, errPlanSelection
			}
			volume := *selected.Volume
			binding.Volume = &volume
		}
		record.Bindings.Sources[name] = binding
	}
	for _, set := range plan.Sets {
		for _, name := range set.Stop {
			selected, ok := service(name)
			if !ok {
				return record, errPlanSelection
			}
			record.Bindings.Stops[name] = selected.Incarnation
		}
		if _, ok := record.Bindings.Repositories[set.Repository]; ok {
			continue
		}
		repository, err := m.storage().load(set.Repository, hostID)
		if err != nil || repository == nil || repository.Metadata.State != "ready" || repository.Metadata.Format != 2 || !digestPattern.MatchString(repository.Metadata.RepositoryID) {
			return record, errPlanRepository
		}
		configDigest, err := digestJSON(repository.Metadata.Config)
		if err != nil {
			return record, errPlanRepository
		}
		record.Bindings.Repositories[set.Repository] = RepositoryPin{ID: repository.Metadata.RepositoryID, ConfigDigest: configDigest}
	}
	digest, err := PlanRevision(record)
	if err != nil {
		return record, errPlanState
	}
	record.Revision = digest
	if ValidatePlanRecord(record) != nil {
		return record, errPlanSelection
	}
	return record, nil
}

// ExecutePlan runs under the caller's existing host mutation lock for preview
// and apply. Show reads recorded policy only. None of these operations captures
// data, changes a runtime, inspects source file content, or contacts a repository.
func (m *Manager) ExecutePlan(ctx context.Context, hostID, app string, r PlanRequest, runtime PlanRuntime) (PlanResponse, error) {
	response := PlanResponse{CurrentRevision: "none", Warnings: []string{
		"Plan configuration only: no application backup, restore, schedule, or downtime is authorized.",
		"Repository pins are recorded metadata; storage access and source prerequisites are not verified by plan setup.",
		"Coverage is unclassified; future capture must report omitted sources and persistent stores.",
	}}
	if ctx.Err() != nil || !ValidOperation(hostID) || ValidatePlanRequest(app, r) != nil {
		return response, errPlanInput
	}
	current, err := m.loadPlan(app, hostID)
	if err != nil {
		return response, err
	}
	response.Current = current
	if current != nil {
		response.CurrentRevision = current.Revision
	}
	if r.Action == "show" {
		return response, nil
	}
	plan, err := NormalizePlan(*r.Plan)
	if err != nil {
		return response, errPlanInput
	}
	proposed, err := m.resolvePlan(hostID, plan, runtime)
	if err != nil {
		return response, err
	}
	response.Proposed = &proposed
	response.Digest = proposed.Revision
	for _, set := range plan.Sets {
		if set.Consistency == "quiesced" {
			response.Warnings = append(response.Warnings, "Declared writers must be stopped under separately approved capture downtime; external database writers cannot be inferred.")
			break
		}
	}
	if r.Action == "preview" {
		return response, nil
	}
	if r.ExpectedDigest != proposed.Revision {
		return response, errPlanConflict
	}
	if response.CurrentRevision == proposed.Revision {
		// Replaying a lost successful result is harmless only for the exact same
		// policy AND freshly re-resolved identities. It never overwrites newer policy.
		response.Proposed = nil
		response.Applied = true
		return response, nil
	}
	if r.ExpectedRevision != response.CurrentRevision {
		return response, errPlanConflict
	}
	proposed.UpdatedAt = m.timestamp()
	if err := m.savePlan(proposed, r.ExpectedRevision); err != nil {
		return response, err
	}
	response.Current = &proposed
	response.CurrentRevision = proposed.Revision
	response.Proposed = nil
	response.Applied = true
	return response, nil
}

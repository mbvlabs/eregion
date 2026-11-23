package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
)

// environmentWorkloadNodes lists every node that may still hold this
// environment's containers: its current node and the runtime node recorded on
// each deployment's revision. A deployment's instances do not carry a node
// name, so the pinned revision is the durable record of where they ran. This
// reaches containers left behind after `env update --node`.
func (s *Service) environmentWorkloadNodes(ctx context.Context, envID platform.EnvironmentID, current string) ([]string, error) {
	recorded, err := s.entities.Platform().Deployments().WorkloadNodes(ctx, envID)
	if err != nil {
		return nil, unavailable(err)
	}
	nodes := []string{}
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			nodes = append(nodes, name)
		}
	}
	add(current)
	for _, name := range recorded {
		add(name)
	}
	sort.Strings(nodes)
	return nodes, nil
}

// targetLockKey is the stable PostgreSQL advisory-lock key for one normalized
// target. Session locks in workers and transaction locks in admission use the
// same key, so scale submission waits for an in-flight deployment instead of
// inspecting an ownership row across a lock gap.
func targetLockKey(target string) int64 {
	sum := sha256.Sum256([]byte(target))
	// Preserve the exact 64-bit key bits; advisory locks only compare identity.
	return int64(binary.BigEndian.Uint64(sum[:8])) //nolint:gosec // G115: intentional two's-complement preservation of a 64-bit advisory key.
}

func uniqueTargets(targets []string) []string {
	sorted := append([]string(nil), targets...)
	sort.Strings(sorted)
	unique := sorted[:0]
	for _, target := range sorted {
		if len(unique) == 0 || unique[len(unique)-1] != target {
			unique = append(unique, target)
		}
	}
	return unique
}

// ensureNoActiveDeleteTargets refuses a deletion while another operation for
// the same records is queued, running or cancelling. This is a state check, not
// a queue-row assumption; the target locks then keep new operations out.
func (s *Service) ensureNoActiveDeleteTargets(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, app, environment string) error {
	var (
		active int64
		err    error
	)
	if environment == "" {
		active, err = tx.Operations().CountActiveDeleteTargetsForApp(ctx, entities.DeleteTargetGuard{
			ExcludeID: id, AppTarget: "app/" + app, EnvironmentPattern: "env/" + app + "/%",
		})
	} else {
		active, err = tx.Operations().CountActiveDeleteTargetsForEnvironment(ctx, entities.EnvironmentDeleteTargetGuard{
			ExcludeID: id, AppTarget: "app/" + app, EnvironmentTarget: "env/" + app + "/" + environment,
			EnvironmentDeleteKind: platform.KindEnvironmentDelete, EnvironmentPattern: "env/" + app + "/%",
		})
	}
	if err != nil {
		return unavailable(err)
	}
	if active > 0 {
		return contract.NewFault(contract.Conflict, "another operation is still active for this target",
			"Wait for it to finish or cancel it explicitly, then delete again.")
	}
	owners, err := tx.Platform().Owners().CountForAppEnvironment(ctx, app, environment)
	if err != nil {
		return unavailable(err)
	}
	if owners > 0 {
		return contract.NewFault(contract.Conflict, "deployment ownership is still settling",
			"Wait for reconciliation to restore the terminal traffic target, then submit the deletion again.")
	}
	return nil
}

// RequiresHumanFactor reports whether a real submission of this normalized
// action currently needs the D7 factor. Authorization still happens inside the
// submission transaction against locked state; this read is for transports
// that must refuse before they can carry a code (the HTTP API).
func (s *Service) RequiresHumanFactor(ctx context.Context, normalized platform.Normalized) (bool, error) {
	switch input := normalized.Input.(type) {
	case *platform.AppDeleteInput:
		return true, nil
	case *platform.EnvironmentDeleteInput:
		env, err := s.environment(ctx, input.App, input.Environment)
		if err != nil {
			return false, err
		}
		counts, err := s.deletionCounts(ctx, input.App, input.Environment)
		if err != nil {
			return false, err
		}
		return platform.EnvironmentDeleteRequiresHumanFactor(env.Protected, counts.appEnvironments == 1), nil
	case *platform.EnvironmentProtectInput:
		env, err := s.environment(ctx, input.App, input.Environment)
		if err != nil {
			return false, err
		}
		return platform.ProtectionChangeRequiresHumanFactor(env.Protected, input.Protected), nil
	default:
		return false, nil
	}
}

func (w *platformWorker) runDelete(ctx context.Context, id contract.OperationID, attempt uint32, normalized platform.Normalized) error {
	switch input := normalized.Input.(type) {
	case *platform.AppDeleteInput:
		return w.deleteApplication(ctx, id, attempt, input.App)
	case *platform.EnvironmentDeleteInput:
		return w.deleteEnvironment(ctx, id, attempt, input.App, input.Environment)
	default:
		return w.settle(ctx, id, attempt, contract.NewFault(contract.Internal, "deletion input is not recognized", "Do not retry accepted work."))
	}
}

func (w *platformWorker) deleteEnvironment(ctx context.Context, id contract.OperationID, attempt uint32, app, environment string) error {
	env, err := w.service.environment(ctx, app, environment)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.deletePreflight(ctx, id, attempt, []platform.EnvironmentID{env.ID}); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	nodes, err := w.service.environmentWorkloadNodes(ctx, env.ID, env.Node)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.runDeleteNodeSteps(ctx, id, attempt, nodes, env.App, env.Name); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.checkDomainDNSBoundary(ctx, id, attempt); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.deleteDomainRowsOwned(ctx, id, attempt, []platform.EnvironmentID{env.ID}); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.syncRoutes(ctx); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	hostRelease, err := w.service.acquireRegistryHostLock(ctx)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	defer hostRelease()
	if err := w.service.finishDomainOperation(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		lockedApp, err := appByNameForUpdateTx(ctx, tx, env.App)
		if err != nil {
			return err
		}
		if err := deleteEnvironmentRowsTx(ctx, tx, env); err != nil {
			return err
		}
		if err := deleteEmptyApplicationTx(ctx, tx, env.App); err != nil {
			return err
		}
		appDeleted, err := tx.Platform().Apps().MissingByID(ctx, lockedApp.ID)
		if err != nil {
			return err
		}
		if data, err := deletionActionData(lockedApp, []platform.Environment{env}, appDeleted); err == nil {
			w.service.tryDeletionOutputTx(ctx, tx, op, data)
		}
		return appendEvent(ctx, tx, op, contract.Event{
			Kind: "log", Stage: "delete",
			Message: "Environment " + env.App + "/" + env.Name + " deleted after workload and route cleanup; operation history was kept.",
		})
	}); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	return nil
}

func (w *platformWorker) deleteApplication(ctx context.Context, id contract.OperationID, attempt uint32, appName string) error {
	app, environments, err := w.service.applicationEnvironments(ctx, appName)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	environmentIDs := make([]platform.EnvironmentID, 0, len(environments))
	for _, env := range environments {
		environmentIDs = append(environmentIDs, env.ID)
	}
	if err := w.deletePreflight(ctx, id, attempt, environmentIDs); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	nodes, err := w.deleteWorkloadNodes(ctx, environments)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.runDeleteNodeSteps(ctx, id, attempt, nodes, app.Name, ""); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.checkDomainDNSBoundary(ctx, id, attempt); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.deleteDomainRowsOwned(ctx, id, attempt, environmentIDs); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.syncRoutes(ctx); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	hostRelease, err := w.service.acquireRegistryHostLock(ctx)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	defer hostRelease()
	if err := w.service.finishDomainOperation(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		return w.deleteApplicationTx(ctx, tx, op, app, environments)
	}); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	return nil
}

func (w *platformWorker) deleteApplicationTx(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, app platform.App, environments []platform.Environment) error {
	lockedApp, err := appByNameForUpdateTx(ctx, tx, app.Name)
	if err != nil {
		return err
	}
	if lockedApp.ID != app.ID {
		return pointerIntegrityFault()
	}
	for _, env := range environments {
		if err := deleteEnvironmentRowsTx(ctx, tx, env); err != nil {
			return err
		}
	}
	if err := deleteApplicationRowsTx(ctx, tx, app); err != nil {
		return err
	}
	if data, err := deletionActionData(lockedApp, environments, true); err == nil {
		w.service.tryDeletionOutputTx(ctx, tx, op, data)
	}
	return appendEvent(ctx, tx, op, contract.Event{
		Kind: "log", Stage: "delete",
		Message: "Application " + app.Name + " and every environment were deleted after workload and route cleanup; operation history was kept.",
	})
}

// deletePreflight verifies the local registry host, reads the operation, runs
// provider cleanup for every managed binding and rechecks the DNS boundary
// before any serving workload is removed.
func (w *platformWorker) deletePreflight(ctx context.Context, id contract.OperationID, attempt uint32, environmentIDs []platform.EnvironmentID) error {
	if _, err := w.service.verifyLocalRegistryHost(ctx); err != nil {
		return err
	}
	op, _, err := w.service.operationSnapshot(ctx, id)
	if err != nil {
		return err
	}
	if err := w.service.cleanupDomainDNSForEnvironments(ctx, op, attempt, environmentIDs); err != nil {
		return err
	}
	if err := w.service.checkDomainDNSBoundary(ctx, id, attempt); err != nil {
		return err
	}
	return nil
}

func (w *platformWorker) deleteWorkloadNodes(ctx context.Context, environments []platform.Environment) ([]string, error) {
	byNode := map[string]bool{}
	for _, env := range environments {
		nodes, err := w.service.environmentWorkloadNodes(ctx, env.ID, env.Node)
		if err != nil {
			return nil, err
		}
		for _, nodeName := range nodes {
			byNode[nodeName] = true
		}
	}
	nodes := make([]string, 0, len(byNode))
	for name := range byNode {
		nodes = append(nodes, name)
	}
	sort.Strings(nodes)
	return nodes, nil
}

func (w *platformWorker) runDeleteNodeSteps(ctx context.Context, id contract.OperationID, attempt uint32, nodes []string, app, environment string) error {
	for index, nodeName := range nodes {
		if err := w.service.checkDomainDNSBoundary(ctx, id, attempt); err != nil {
			return err
		}
		tr, err := w.service.resolver.TransportFor(ctx, nodeName)
		if err != nil {
			return err
		}
		step := fmt.Sprintf("platform-delete-%02d-v1", index+1)
		if _, err := w.runStepOn(ctx, tr, id, attempt, step, platform.DeletePayload{
			Step: step, App: app, Environment: environment,
		}, nil); err != nil {
			return err
		}
	}
	return nil
}

func deleteEnvironmentRowsTx(ctx context.Context, tx *entities.StoreTx, env platform.Environment) error {
	appID, err := lockPointerEnvironmentTx(ctx, tx, env.ID)
	if err != nil {
		return err
	}
	name, found, err := tx.Platform().Apps().NameByID(ctx, platform.AppID(appID))
	if err != nil || !found || name != env.App {
		return pointerIntegrityFault()
	}
	// Never cascade through a corrupt cross-environment reference. All supported
	// pointer writers validate ownership while holding the target environment.
	foreign, err := tx.Platform().Environments().HasForeignReferences(ctx, env.ID)
	if err != nil {
		return unavailable(err)
	}
	if foreign {
		return pointerIntegrityFault()
	}
	if _, err := tx.Platform().Environments().ClearPointers(ctx, env.ID); err != nil {
		return unavailable(err)
	}
	if _, err := tx.Platform().Deployments().DeleteForEnvironment(ctx, env.ID); err != nil {
		return unavailable(err)
	}
	if _, err := tx.Platform().Releases().DeleteForEnvironment(ctx, env.ID); err != nil {
		return unavailable(err)
	}
	if _, err := tx.Platform().Builds().DeleteForEnvironment(ctx, env.ID); err != nil {
		return unavailable(err)
	}
	if _, err := tx.Platform().Revisions().DeleteForEnvironment(ctx, env.ID); err != nil {
		return unavailable(err)
	}
	if _, err := tx.Platform().Domains().DeleteForEnvironment(ctx, env.ID); err != nil {
		return unavailable(err)
	}
	if _, err := tx.Platform().Changes().DeleteForEnvironment(ctx, env.ID); err != nil {
		return unavailable(err)
	}
	if _, err := tx.Secrets().DeleteForEnvironment(ctx, env.App, env.Name); err != nil {
		return unavailable(err)
	}
	if _, err := tx.Platform().Environments().Delete(ctx, env.ID); err != nil {
		return unavailable(err)
	}
	return nil
}

func deleteEmptyApplicationTx(ctx context.Context, tx *entities.StoreTx, app string) error {
	appID, found, err := tx.Platform().Apps().IDByName(ctx, app)
	if err != nil {
		return unavailable(err)
	}
	if !found {
		return nil
	}
	remaining, err := tx.Platform().Environments().CountForApp(ctx, appID)
	if err != nil {
		return unavailable(err)
	}
	if remaining != 0 {
		return nil
	}
	return deleteApplicationRowsTx(ctx, tx, platform.App{ID: appID, Name: app})
}

func deleteApplicationRowsTx(ctx context.Context, tx *entities.StoreTx, app platform.App) error {
	if app.ID == "" {
		return contract.NewFault(contract.Internal, "application identity is missing for deletion", "Reload the application and retry; deletion is bound to the durable app ID, not its name alone.")
	}
	// Do not let the app cascade bypass per-environment pointer validation.
	remaining, err := tx.Platform().Environments().CountForApp(ctx, app.ID)
	if err != nil {
		return unavailable(err)
	}
	if remaining != 0 {
		return contract.NewFault(contract.Conflict, "application still has environments", "Reload the application and retry deletion.")
	}
	// Secret namespaces are not foreign keys to platform apps.
	deleted, err := tx.Platform().Apps().Delete(ctx, app.ID)
	if err != nil {
		return unavailable(err)
	}
	if deleted == 0 {
		// The identity-bound row is already gone; never erase another app's
		// same-named secret namespace.
		return nil
	}
	if _, err := tx.Secrets().DeleteForApp(ctx, app.Name); err != nil {
		return unavailable(err)
	}
	return nil
}

// deleteDomainRowsOwned performs the same deletion under the operation row
// lock, so a cancellation accepted before the boundary retains the rows.
func (s *Service) deleteDomainRowsOwned(ctx context.Context, id contract.OperationID, attempt uint32, environmentIDs []platform.EnvironmentID) error {
	if len(environmentIDs) == 0 {
		return nil
	}
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		if op.State == contract.Cancelling {
			return errPlatformCancelled
		}
		return deleteDomainRowsTx(ctx, tx, environmentIDs)
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

func deleteDomainRowsTx(ctx context.Context, tx *entities.StoreTx, environmentIDs []platform.EnvironmentID) error {
	ids := make([]string, len(environmentIDs))
	for index, id := range environmentIDs {
		ids[index] = string(id)
	}
	if _, err := tx.Platform().DomainDNS().DeleteCleanupForEnvironments(ctx, ids); err != nil {
		return unavailable(err)
	}
	if _, err := tx.Platform().Domains().DeleteForEnvironments(ctx, ids); err != nil {
		return unavailable(err)
	}
	return nil
}

// deletionPreview builds the dry-run plan without deleting anything.
func (s *Service) deletionPreview(ctx context.Context, normalized platform.Normalized) (platform.Preview, error) {
	input := normalized.Input
	preview := platform.Preview{Kind: normalized.Kind, Target: normalized.Target.String(), DryRun: true, Input: input.Preview(), Summary: normalized.Summary}
	var app, environment string
	var appID, envID string
	protected, lastEnvironment := false, false
	switch value := input.(type) {
	case *platform.AppDeleteInput:
		app = value.App
		if err := s.ensureApplication(ctx, app); err != nil {
			return preview, err
		}
		id, found, err := s.entities.Platform().Apps().IDByName(ctx, app)
		if err != nil {
			return preview, unavailable(err)
		}
		if !found {
			return preview, contract.NewFault(contract.NotFound, "application not found", "List applications with eregion app list.")
		}
		appID = string(id)
	case *platform.EnvironmentDeleteInput:
		app, environment = value.App, value.Environment
		env, err := s.environment(ctx, app, environment)
		if err != nil {
			return preview, err
		}
		protected = env.Protected
		envID = string(env.ID)
		counts, err := s.deletionCounts(ctx, app, environment)
		if err != nil {
			return preview, err
		}
		lastEnvironment = counts.appEnvironments == 1
	default:
		return preview, contract.NewFault(contract.Internal, "deletion preview input is not recognized", "Do not retry accepted work.")
	}
	counts, err := s.deletionCounts(ctx, app, environment)
	if err != nil {
		return preview, err
	}
	preview.Steps = platform.DeletionSteps(app, environment)
	preview.Summary = deletionSummary(normalized.Target.String(), counts)
	preview.Warnings = append(preview.Warnings,
		"Containers selected by ownership labels and their Caddy routes are removed before any database row.",
		"Durable operation and event history is kept; only platform state and secret values are deleted.",
		"Application and environment deletion never removes resource data volumes, including volumes retained after resource destruction.",
	)
	preview.Warnings = append(preview.Warnings, deletionWarnings(counts, environment)...)
	blockers, err := liveDeletionResources(ctx, s.entities.Resources(), platform.AppID(appID), platform.EnvironmentID(envID))
	if err != nil {
		return preview, err
	}
	for _, blocker := range blockers {
		preview.Warnings = append(preview.Warnings, fmt.Sprintf("Deletion blocked by resource %s/%s/%s (state: %s). Destroy with: eregion resource destroy %s %s %s --confirm %s/%s/%s.", app, blocker.environment, blocker.name, blocker.state, app, blocker.environment, blocker.name, app, blocker.environment, blocker.name))
	}
	if len(blockers) > 0 {
		preview.Warnings = append(preview.Warnings, "Detach each resource from the current revision and ensure the serving revision no longer uses it; wait for dependent deployments before destroying it. Default destroy retains named data volumes. Use --delete-data only as a separate explicit choice to permanently remove owned data.")
	}
	switch {
	case environment == "":
		preview.Warnings = append(preview.Warnings, "Every application deletion requires the D7 TOTP human factor, regardless of environment protection.")
	case platform.EnvironmentDeleteRequiresHumanFactor(protected, lastEnvironment):
		reason := "This environment is protected"
		if lastEnvironment && !protected {
			reason = "This is the last environment, so deleting it also deletes the application"
		}
		preview.Warnings = append(preview.Warnings, reason+"; the real deletion requires the D7 TOTP human factor at a human terminal.")
	default:
		preview.Warnings = append(preview.Warnings, "This environment is unprotected and is not the last one; the real deletion needs only the exact target confirmation and is available to agents, JSON and no-TTY calls.")
	}
	return preview, nil
}

// deletionSummary names the rows the target deletion removes. Every count is
// scoped to the target environment when one is named; appEnvironments is only
// used by the last-environment warning.
func deletionSummary(target string, counts deletionCounts) string {
	return fmt.Sprintf("Delete %s: %d environment(s), %d release(s), %d deployment(s), %d build(s), %d domain(s), %d secret namespace(s).",
		target, counts.environments, counts.releases, counts.deployments, counts.builds, counts.domains, counts.secrets)
}

// deletionWarnings states the scope of the deletion. counts.environments is
// target-scoped, so the last-environment warning must use the application-wide
// count instead; otherwise every named-environment delete would claim to be
// the last one.
func deletionWarnings(counts deletionCounts, environment string) []string {
	if environment == "" {
		return []string{"Every environment of the application is deleted."}
	}
	if counts.appEnvironments == 1 {
		return []string{"This is the app's last environment; deleting it also deletes the application record."}
	}
	return nil
}

type deletionCounts struct {
	environments    int
	appEnvironments int
	releases        int
	deployments     int
	builds          int
	domains         int
	secrets         int
}

func (s *Service) deletionCounts(ctx context.Context, app, environment string) (deletionCounts, error) {
	var counts deletionCounts
	record, err := s.entities.Platform().Outcomes().DeletionCounts(ctx, app, environment)
	if err != nil {
		return counts, unavailable(err)
	}
	counts.environments = int(record.Environments)
	counts.releases = int(record.Releases)
	counts.deployments = int(record.Deployments)
	counts.builds = int(record.Builds)
	counts.domains = int(record.Domains)
	counts.secrets = int(record.Secrets)
	counts.appEnvironments = int(record.AppEnvironments)
	return counts, nil
}

func (s *Service) ensureApplication(ctx context.Context, app string) error {
	exists, err := s.entities.Platform().Apps().Exists(ctx, app)
	if err != nil {
		return unavailable(err)
	}
	if !exists {
		return contract.NewFault(contract.NotFound, "application not found", "List applications with eregion app list.")
	}
	return nil
}

func (s *Service) applicationEnvironments(ctx context.Context, app string) (platform.App, []platform.Environment, error) {
	record, found, err := s.entities.Platform().Apps().GetByName(ctx, app)
	if err != nil {
		return record, nil, unavailable(err)
	}
	if !found {
		return record, nil, contract.NewFault(contract.NotFound, "application not found", "List applications with eregion app list.")
	}
	observations, err := s.entities.Platform().Environments().List(ctx, entities.EnvironmentFilter{
		App: app, Limit: int(^uint32(0) >> 1),
	})
	if err != nil {
		return record, nil, unavailable(err)
	}
	environments := make([]platform.Environment, 0, len(observations))
	for _, observation := range observations {
		environments = append(environments, observation.Environment)
	}
	return record, environments, nil
}

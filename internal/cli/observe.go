package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/spf13/cobra"
)

// appStatusView is the extended `app status`: every environment carries the
// serving release, desired revision, per-process replicas with Docker state,
// domain observations, deployability and the latest operations.
func (a *app) appStatusView(cmd *cobra.Command, app string) (result, error) {
	if err := platform.ValidateName(app); err != nil {
		return result{}, err
	}
	appRaw, err := a.platformReadInventoryWith(cmd, "apps", urlValues("app", app, "limit", "1"))
	if err != nil {
		return result{}, err
	}
	appItem, err := platformSelectItem(appRaw, app)
	if err != nil {
		return result{}, err
	}
	var record platform.App
	if json.Unmarshal(appItem, &record) != nil || record.Name != app {
		return result{}, invalidNodeData("application")
	}
	envRaw, err := a.platformReadInventoryWith(cmd, "environments", urlValues("app", app, "limit", "200"))
	if err != nil {
		return result{}, err
	}
	environments, err := decodeInventoryItems[platform.Environment](envRaw)
	if err != nil {
		return result{}, err
	}
	releases := a.readStatusReleases(cmd, app, "")
	workloads, workloadErr := a.readStatusWorkloads(cmd, app, "")
	domains, domainsErr := a.readStatusDomains(cmd, app, "")
	operationsList, operationsErr := a.readStatusOperations(cmd, app, "", 10)
	observation := platform.AppObservation{App: record, Environments: []platform.EnvironmentObservation{}, Operations: operationsList, OperationsError: operationsErr}
	for _, environment := range environments {
		item := platform.BuildEnvironmentObservation(environment, findRelease(releases, environment.ServingRelease), workloads.Containers, domains, nil)
		item.WorkloadError = workloadErr
		item.DomainsError = domainsErr
		if workloadErr != "" {
			markWorkloadUnknown(&item)
		}
		observation.Environments = append(observation.Environments, item)
	}
	var plain strings.Builder
	plain.WriteString(ui.Header(fmt.Sprintf("App %s (%s)", record.Name, record.ID), 72) + "\n")
	if len(observation.Environments) == 0 {
		plain.WriteString("No environments. Add one with eregion env create.\n")
		return result{data: observation, plain: plain.String()}, nil
	}
	if workloadErr != "" {
		fmt.Fprintf(&plain, "%s\n", ui.Warn("container state is unknown: "+workloadErr))
	}
	for _, item := range observation.Environments {
		plain.WriteString("\n")
		plain.WriteString(writeEnvironmentObservation(item))
	}
	if len(observation.Operations) > 0 {
		plain.WriteString("\n" + ui.Section("Last operations for the app") + "\n")
		plain.WriteString(writeOperationTable(observation.Operations))
	}
	return result{data: observation, plain: plain.String()}, nil
}

// envStatusView is the extended `env status`.
func (a *app) envStatusView(cmd *cobra.Command, app, environment string) (result, error) {
	env, _, err := a.readEnvironment(cmd, app, environment)
	if err != nil {
		return result{}, err
	}
	releases := a.readStatusReleases(cmd, app, environment)
	workloads, workloadErr := a.readStatusWorkloads(cmd, app, environment)
	domains, domainsErr := a.readStatusDomains(cmd, app, environment)
	operationsList, operationsErr := a.readStatusOperations(cmd, app, environment, 10)
	observation := platform.BuildEnvironmentObservation(env, findRelease(releases, env.ServingRelease), workloads.Containers, domains, operationsList)
	observation.WorkloadError = workloadErr
	observation.DomainsError = domainsErr
	observation.OperationsError = operationsErr
	if workloadErr != "" {
		markWorkloadUnknown(&observation)
	}
	return result{data: observation, plain: writeEnvironmentObservation(observation)}, nil
}

// markWorkloadUnknown keeps an unreadable Docker observation honest: a replica
// whose container could not be listed is unknown, not missing.
func markWorkloadUnknown(observation *platform.EnvironmentObservation) {
	for processIndex := range observation.Processes {
		for replicaIndex := range observation.Processes[processIndex].Replicas {
			replica := &observation.Processes[processIndex].Replicas[replicaIndex]
			if replica.State == "missing" {
				replica.State = platform.ObservationUnknown
			}
		}
	}
}

func (a *app) readStatusReleases(cmd *cobra.Command, app, environment string) []platform.Release {
	filters := urlValues("app", app, "limit", "200")
	if environment != "" {
		filters["environment"] = []string{environment}
	}
	raw, err := a.platformReadInventoryWith(cmd, "releases", filters)
	if err != nil {
		return nil
	}
	releases, err := decodeInventoryItems[platform.Release](raw)
	if err != nil {
		return nil
	}
	return releases
}

func (a *app) readStatusWorkloads(cmd *cobra.Command, app, environment string) (platform.Workloads, string) {
	client, err := a.client(cmd)
	if err != nil {
		return platform.Workloads{}, contract.Classify(err).Message
	}
	workloads, err := client.Workloads(cmd.Context(), app, environment)
	if err != nil {
		return platform.Workloads{}, contract.Classify(err).Message
	}
	return workloads, ""
}

func (a *app) readStatusDomains(cmd *cobra.Command, app, environment string) ([]platform.DomainBinding, string) {
	filters := urlValues("app", app, "limit", "200")
	if environment != "" {
		filters["environment"] = []string{environment}
	}
	raw, err := a.platformReadInventoryWith(cmd, "domains", filters)
	if err != nil {
		return nil, contract.Classify(err).Message
	}
	domains, err := decodeInventoryItems[platform.DomainBinding](raw)
	if err != nil {
		return nil, err.Error()
	}
	return domains, ""
}

func (a *app) readStatusOperations(cmd *cobra.Command, app, environment string, limit int) ([]contract.Operation, string) {
	client, err := a.client(cmd)
	if err != nil {
		return nil, contract.Classify(err).Message
	}
	page, err := client.ListOperations(cmd.Context(), operations.ListQuery{App: app, Environment: environment, Limit: limit})
	if err != nil {
		return nil, contract.Classify(err).Message
	}
	return page.Items, ""
}

func findRelease(releases []platform.Release, id platform.ReleaseID) *platform.Release {
	if id == "" {
		return nil
	}
	for index := range releases {
		if releases[index].ID == id {
			return &releases[index]
		}
	}
	return nil
}

func writeEnvironmentObservation(observation platform.EnvironmentObservation) string {
	env := observation.Environment
	var out strings.Builder
	fmt.Fprintf(&out, "%s\n", ui.Header(fmt.Sprintf("Environment %s/%s (%s)", env.App, env.Name, env.ID), 72))
	field := func(name, value string) {
		fmt.Fprintf(&out, "  %s %s\n", ui.Dim(fmt.Sprintf("%-17s", name+":")), value)
	}
	field("node", env.Node)
	if env.Revision != nil {
		field("desired revision", fmt.Sprintf("%d (%s)", env.Revision.Number, env.Revision.ID))
		field("source", describeSource(env.Revision.Source))
	} else {
		field("desired revision", "none")
	}
	if env.ServingRelease != "" {
		details := string(env.ServingRelease)
		if observation.ServingRelease != nil {
			release := observation.ServingRelease
			details = string(release.ID) + " " + release.Image
			if release.CommitSHA != "" {
				details += " commit " + release.CommitSHA
			}
		}
		field("serving release", details)
	} else {
		field("serving release", "none")
	}
	field("deployable", ui.State(environmentDeployability(env)))
	health := env.HealthState
	if health == "" {
		health = platform.HealthUnknown
	}
	field("health", ui.State(string(health)))
	if observation.WorkloadError != "" {
		field("workload observation", ui.State("unknown")+" ("+observation.WorkloadError+")")
	}
	rows := make([][]string, 0)
	for _, process := range observation.Processes {
		for _, replica := range process.Replicas {
			container := replica.Container
			if container == "" {
				container = "-"
			}
			port := "-"
			if replica.HostPort > 0 {
				port = fmt.Sprint(replica.HostPort)
			}
			serving := "no"
			if replica.Serving {
				serving = "yes"
			}
			rows = append(rows, []string{process.Name, process.Kind, replica.Replica, replica.State, container, port, serving})
		}
	}
	for _, replica := range observation.OtherContainers {
		container := replica.Container
		if container == "" {
			container = "-"
		}
		port := "-"
		if replica.HostPort > 0 {
			port = fmt.Sprint(replica.HostPort)
		}
		serving := "no"
		if replica.Serving {
			serving = "yes"
		}
		rows = append(rows, []string{"other", "unexpected", replica.Replica, replica.State, container, port, serving})
	}
	out.WriteString(ui.Section("Processes") + "\n")
	if len(rows) == 0 {
		out.WriteString("  (none observed and none desired)\n")
	} else {
		out.WriteString(writeTable([]string{"PROCESS", "KIND", "REPLICA", "STATE", "CONTAINER", "HOST PORT", "SERVING"}, rows))
	}
	if observation.DomainsError != "" {
		fmt.Fprintf(&out, "%s\n", ui.Warn("domains unavailable: "+observation.DomainsError))
	} else if len(observation.Domains) > 0 {
		out.WriteString(ui.Section("Domains") + "\n")
		domainRows := make([][]string, 0, len(observation.Domains))
		for _, domain := range observation.Domains {
			primary := "no"
			if domain.Primary {
				primary = "yes"
			}
			ready := "no"
			if domain.Ready {
				ready = "yes"
			}
			domainRows = append(domainRows, []string{
				domain.Domain, primary, string(domain.RouteState), string(domain.DNSState),
				string(domain.CertificateState), string(domain.PublicState), ready,
			})
			if message := domainBindingError(domain.DomainBinding); message != "" {
				domainRows = append(domainRows, []string{domain.Domain, "", "", "", "", "", message})
			}
		}
		out.WriteString(writeTable([]string{"DOMAIN", "PRIMARY", "ROUTE", "DNS", "CERTIFICATE", "PUBLIC", "READY"}, domainRows))
	}
	if observation.OperationsError != "" {
		fmt.Fprintf(&out, "%s\n", ui.Warn("last operations unavailable: "+observation.OperationsError))
	} else if len(observation.Operations) > 0 {
		out.WriteString(ui.Section("Last operations") + "\n")
		out.WriteString(writeOperationTable(observation.Operations))
	}
	return out.String()
}

func writeOperationTable(items []contract.Operation) string {
	rows := make([][]string, 0, len(items))
	for _, operation := range items {
		rows = append(rows, []string{
			string(operation.ID), operation.Kind, operation.Target, string(operation.State),
			operation.UpdatedAt.UTC().Format("2006-01-02 15:04:05Z"),
		})
	}
	return writeTable([]string{"OPERATION", "KIND", "TARGET", "STATE", "UPDATED"}, rows)
}

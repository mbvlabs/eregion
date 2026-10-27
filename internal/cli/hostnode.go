package cli

import (
	"context"
	"io"
	"os"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/doctor"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/rpc"
	"github.com/mbvlabs/eregion/internal/secrets"
)

func isHostMethod(method string) bool {
	switch method {
	case methodHostPlan, methodHostApply, methodHostDoctor, methodHostStatus:
		return true
	}
	return false
}

// dispatchHostMethod runs the node-local host surface. These methods do not
// need the application service, so plan and doctor still work while the
// control database has pending migrations or the API is down.
func dispatchHostMethod(ctx context.Context, request rpc.Request, progress io.Writer, registry *secrets.Registry) (any, error) {
	if registry == nil {
		registry = &secrets.Registry{}
	}
	envErr := host.LoadServiceEnv(host.ServiceEnvPath)
	dsn := ""
	if envErr == nil {
		dsn = os.Getenv("EREGION_DATABASE_URL")
	}
	registry.Add(dsn)
	layout := host.DefaultLayout()
	cfg, cfgErr := host.LoadNodeConfig(layout, dsn)
	if cfgErr != nil && envErr != nil {
		cfgErr = contract.NewFault(contract.Unavailable, "the node service configuration is not readable", envErr.Error())
	}
	if cfg.Layout.DataDir == "" {
		cfg.Layout = layout
	}
	if cfg.DatabaseURL == "" {
		cfg.DatabaseURL = dsn
	}

	switch request.Method {
	case methodHostPlan, methodHostApply:
		if cfgErr != nil {
			return nil, cfgErr
		}
		runner, err := host.NewRunner(cfg, registry)
		if err != nil {
			return nil, err
		}
		runner.Reporter = hostProgress(progress, registry)
		if request.Method == methodHostPlan || request.DryRun {
			return runner.Plan(ctx)
		}
		confirm := strings.TrimSpace(request.Confirm)
		if confirm == "" {
			plan, err := runner.Plan(ctx)
			if err != nil {
				return nil, err
			}
			if !plan.Supported {
				return nil, contract.NewFault(contract.PreconditionFailed, "the node platform is not supported", "Use Debian 13 or Ubuntu 26.04 on amd64 or arm64.")
			}
			confirm = plan.Confirmation.Token
		}
		return runner.Apply(ctx, confirm, false)
	case methodHostDoctor:
		var drift host.Plan
		var driftErr error
		if cfgErr == nil {
			if runner, err := host.NewRunner(cfg, registry); err == nil {
				drift, driftErr = runner.Plan(ctx)
			} else {
				driftErr = err
			}
		}
		facts := host.GatherFacts(ctx, cfg, cfgErr, drift, driftErr, request.ClientVersion, request.ClientCommit, registry)
		return doctor.BuildReport(doctor.Run(doctor.Checks(), facts)), nil
	case methodHostStatus:
		facts := host.GatherFacts(ctx, cfg, cfgErr, host.Plan{}, nil, request.ClientVersion, request.ClientCommit, registry)
		return host.StatusFromFacts(facts), nil
	default:
		return nil, contract.NewFault(contract.Usage, "unknown host method "+request.Method, "Use host.plan, host.apply, host.doctor, or host.status.")
	}
}

func hostProgress(out io.Writer, registry *secrets.Registry) engine.Reporter {
	if out == nil {
		return func(engine.Event) {}
	}
	return func(event engine.Event) {
		message := registry.Redact(event.Message)
		if strings.TrimSpace(message) == "" {
			message = event.Step
		}
		line, err := rpc.MarshalEvent(rpc.EventBody{Kind: event.Kind, Step: event.Step, Message: message})
		if err != nil {
			return
		}
		_, _ = out.Write(line)
	}
}

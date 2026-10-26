package operations

import (
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
)

// ListQuery is one bounded operation history read. With no app filter the
// history follows the caller's authority: a bare actor sees its own operations,
// while the SSH-authenticated operator channel sees every operation on the
// node. App selects the app record and every environment below it, plus their
// build operations; Environment narrows that to one environment. An
// app/environment filter also includes operations submitted by CI tokens and
// the push webhook.
type ListQuery struct {
	App         string
	Environment string
	State       string
	Limit       int
}

// ListPage is the operation history page returned by the control plane.
type ListPage struct {
	Items []contract.Operation `json:"items"`
	Limit int                  `json:"limit"`
}

// Validate checks the query before any SQL runs. Environment needs App because
// operation targets are env/<app>/<environment>.
func (q ListQuery) Validate() error {
	if q.App != "" && len(q.App) > 128 {
		return contract.NewFault(contract.ValidationFailed, "app filter is too long", "Use a documented application name.")
	}
	if q.Environment != "" {
		if q.App == "" {
			return contract.NewFault(contract.InputRequired, "an environment filter needs an app", "Pass --app with --environment.")
		}
		if len(q.Environment) > 128 {
			return contract.NewFault(contract.ValidationFailed, "environment filter is too long", "Use a documented environment name.")
		}
	}
	if q.State != "" && !contract.State(strings.TrimSpace(q.State)).Valid() {
		return contract.NewFault(contract.ValidationFailed, "unknown operation state", "Use queued, running, cancelling, succeeded, failed, or cancelled.")
	}
	if q.Limit == 0 {
		return nil
	}
	if q.Limit < 1 || q.Limit > 200 {
		return contract.NewFault(contract.ValidationFailed, "operation list limit out of range", "Use a limit from 1 to 200.")
	}
	return nil
}

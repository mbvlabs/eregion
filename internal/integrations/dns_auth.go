package integrations

import (
	"context"

	"github.com/mbvlabs/eregion/internal/contract"
)

// OperatorDNSAuthorizer is the domain-flow provider authorizer. It grants only
// the exact DNS actions to a validated operator actor. A configured zone is
// never an ACL: every legitimate SSH operator may manage the same zone, while
// CI, webhook and system actors are denied regardless of subject. There is no
// wildcard grant and no environment allowlist for the domain flow.
type OperatorDNSAuthorizer struct{}

// NewOperatorDNSAuthorizer returns the domain-flow authorizer. It is nonnil so
// DomainDNS does not fall back to a broader integration authorizer.
func NewOperatorDNSAuthorizer() OperatorDNSAuthorizer { return OperatorDNSAuthorizer{} }

func (OperatorDNSAuthorizer) Authorize(_ context.Context, actor contract.Actor, action, _ string) error {
	if err := actor.Validate(); err != nil {
		return err
	}
	if actor.Kind != "operator" {
		return contract.NewFault(contract.Forbidden, "only an authenticated operator may manage domain DNS", "Use the SSH operator boundary; CI, webhook and system actors cannot manage provider credentials or records.")
	}
	switch action {
	case ActionDNSRead, ActionDNSWrite, ActionDNSCreate, ActionDNSUpdate, ActionDNSDelete:
		return nil
	default:
		return contract.NewFault(contract.Forbidden, "DNS action is not authorized", "Use one of the documented dns.read, dns.write, dns.create, dns.update or dns.delete actions.")
	}
}

func clearBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

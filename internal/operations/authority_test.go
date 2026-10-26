package operations

import (
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

func TestAuthoritySelection(t *testing.T) {
	actor := contract.Actor{Kind: "operator", Subject: "alice"}
	scoped, all, err := ResolveAuthority(actor)
	if err != nil || scoped != actor || all {
		t.Fatalf("a server-resolved actor must stay scoped: actor=%+v all=%t err=%v", scoped, all, err)
	}
	operator, all, err := ResolveAuthority(OperatorAuthority(actor))
	if err != nil || operator != actor || !all {
		t.Fatalf("operator authority must cover every actor: actor=%+v all=%t err=%v", operator, all, err)
	}
	for _, authority := range []Authority{nil, contract.Actor{Kind: "unknown", Subject: "alice"}, contract.Actor{Kind: "operator"}} {
		if _, _, err := ResolveAuthority(authority); err == nil {
			t.Fatalf("accepted invalid authority %#v", authority)
		}
	}
}

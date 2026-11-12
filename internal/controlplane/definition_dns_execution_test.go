package controlplane

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/integrations"
)

// This structural regression checks the shared lock order at the real SQL
// call sites without opening a database or simulating provider effects.
func TestDefinitionDNSLockOrderStructure(t *testing.T) {
	function := func(file, start, end string) string {
		t.Helper()
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		i := strings.Index(text, start)
		if i < 0 {
			t.Fatalf("missing %s", start)
		}
		text = text[i:]
		j := strings.Index(text[len(start):], end)
		if j < 0 {
			t.Fatalf("missing end %s", end)
		}
		return text[:len(start)+j]
	}
	ordered := func(text string, parts ...string) {
		t.Helper()
		for _, part := range parts {
			i := strings.Index(text, part)
			if i < 0 {
				t.Fatalf("missing or out of order %q", part)
			}
			text = text[i+len(part):]
		}
	}
	loader := function("definition_dns_execution.go", "func loadDefinitionDNSChildTx", "// definitionDNSChild opens")
	ordered(loader, "SELECT a.name,e.name,e.id", "lockDefinitionTargetTx(",
		"SELECT id FROM eregion_operations WHERE id=$1 FOR UPDATE`, parentID",
		"SELECT operation_id FROM eregion_definition_runs WHERE operation_id=$1 FOR UPDATE`, parentID",
		"SELECT parent_operation_id FROM eregion_definition_steps WHERE parent_operation_id=$1 AND ordinal=$2 FOR UPDATE`, parentID, ordinal",
		"SELECT id FROM eregion_operations WHERE id=$1 FOR UPDATE`, child",
		"WHERE s.parent_operation_id=$1 AND s.ordinal=$2 AND s.child_operation_id=$3`", "d.ParentID != parentID || d.Ordinal != ordinal")
	if strings.Contains(loader, "FOR UPDATE OF") || strings.Count(loader, "FOR UPDATE`") != 4 {
		t.Fatal("child loader must use exactly four separate single-row locks")
	}
	progress := function("definition_dns_execution.go", "func (p *definitionDNSProgressStore) withLocked", "func ")
	ordered(progress, "loadDefinitionDNSChildTx(", "lockDefinitionDNSLedgerTx(")
	cleanup := function("definition_dns_execution.go", "func (s *Service) checkDefinitionDNSCleanupAuthority", "func (p *definitionDNSProgressStore) Intent")
	ordered(cleanup, "loadDefinitionDNSChildTx(", "lockDefinitionDNSLedgerTx(")
	ledger := function("definition_dns_execution.go", "func lockDefinitionDNSLedgerTx", "// withLocked")
	ordered(ledger, "FROM eregion_platform_domain_dns WHERE domain_id=$1 FOR UPDATE", "FROM eregion_platform_domains WHERE id=$1 AND environment_id=$2 FOR UPDATE", "domainDNSLedgerSelect+")
	poll := function("definition_worker.go", "func (w *platformWorker) runDefinitionApply", "func (w *platformWorker) createDefinitionChild")
	ordered(poll, "FROM eregion_operations WHERE id=$1 FOR UPDATE", "FOR UPDATE OF r", "FROM eregion_definition_steps WHERE parent_operation_id=$1 AND ordinal=$2 FOR UPDATE", "tx.Commit(ctx)", "w.createDefinitionChild(")
	if strings.Contains(poll, "lockDefinitionTargetTx(") || strings.Contains(poll, "environmentByNameTx(") {
		t.Fatal("parent poll must not acquire the app/environment locks in its transaction")
	}
	worker := function("definition_worker.go", "func (w *platformWorker) createDefinitionChild", "func (w *platformWorker) failDefinitionAfterRollback")
	ordered(worker, "lockDefinitionTargetTx(", "FOR UPDATE", "UPDATE eregion_definition_steps")
	target := function("platform_state.go", "func lockDefinitionTargetTx", "func fenceDefinitionAdmissionTx")
	ordered(target, "appByNameForUpdateTx(", "environmentByNameTx(")
	prepare := function("platform_state.go", "func (s *Service) preparePlatform", "func (s *Service) ")
	ordered(prepare, "fenceDefinitionAdmissionTx(", "loadDefinitionDNSChildTx(")
}

func TestDefinitionDNSCleanupBoundaryRechecksAfterProviderRead(t *testing.T) {
	var calls []string
	authorized := true
	denied := errors.New("authority lost")
	check := func() error {
		calls = append(calls, "check")
		if !authorized {
			return denied
		}
		return nil
	}
	err := definitionDNSCleanupBoundary(check, func() error {
		calls = append(calls, "provider")
		authorized = false
		return nil
	})
	if !errors.Is(err, denied) {
		t.Fatalf("lost authority must block deletion, got %v", err)
	}
	if !reflect.DeepEqual(calls, []string{"check", "provider", "check"}) {
		t.Fatalf("boundary order = %v", calls)
	}
}

func TestDefinitionDNSProgressPrefix(t *testing.T) {
	before := []integrations.DomainDNSRecord{}
	change := integrations.DomainDNSChange{Action: "create", Type: "A"}
	plan := integrations.DefinitionDNSPlan{Before: before, Changes: []integrations.DomainDNSChange{change, change}}
	observed := []integrations.DomainDNSRecord{{ID: "record"}}
	first := integrations.DefinitionDNSProgress{Index: 0, Before: before, Expected: change, Result: observed}
	pending := integrations.DefinitionDNSProgress{Index: 1, Before: observed, Expected: change}
	tests := []struct {
		name    string
		entries []integrations.DefinitionDNSProgress
		valid   bool
	}{
		{"empty", nil, true},
		{"pending", []integrations.DefinitionDNSProgress{{Index: 0, Before: before, Expected: change}}, true},
		{"observed and pending", []integrations.DefinitionDNSProgress{first, pending}, true},
		{"gap", []integrations.DefinitionDNSProgress{pending}, false},
		{"pending not last", []integrations.DefinitionDNSProgress{{Index: 0, Before: before, Expected: change}, pending}, false},
		{"wrong before", []integrations.DefinitionDNSProgress{first, {Index: 1, Before: before, Expected: change}}, false},
		{"wrong expected", []integrations.DefinitionDNSProgress{{Index: 0, Before: before}}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validateDefinitionDNSProgress(plan, test.entries); (got == nil) != test.valid {
				t.Fatalf("validation error = %v, valid = %v", got, test.valid)
			}
		})
	}
}

func TestDefinitionDNSPlanMatchesLedger(t *testing.T) {
	ledger := domainDNSLedger{Domain: "app.example.com", Ownership: "owner", DesiredOrigins: []string{"2001:4860:4860::8888", "8.8.8.8"}, DesiredProxied: true}
	plan := integrations.DefinitionDNSPlan{Desired: []integrations.DNSRecord{
		{Name: ledger.Domain, Type: "A", Content: "8.8.8.8", TTL: 1, Proxied: true, Comment: "owner"},
		{Name: ledger.Domain, Type: "AAAA", Content: "2001:4860:4860::8888", TTL: 1, Proxied: true, Comment: "owner"},
	}}
	if !definitionDNSPlanMatchesLedger(plan, ledger) {
		t.Fatal("matching plan rejected")
	}
	plan.Desired[0].Proxied = false
	if definitionDNSPlanMatchesLedger(plan, ledger) {
		t.Fatal("changed proxy accepted")
	}
}

func TestDefinitionDNSCleanupSubset(t *testing.T) {
	record := integrations.DomainDNSRecord{ID: "one", Name: "app.example.com", Type: "A", Content: "8.8.8.8", TTL: 1, Comment: "owner", Ownership: "owned"}
	change := integrations.DomainDNSChange{Action: "delete", Type: "A", Before: &record}
	approved := integrations.DomainDNSPlan{Zone: "example.com", ZoneID: "zone", DomainID: "domain", Host: record.Name, Ownership: "owner", Changes: []integrations.DomainDNSChange{change}}
	current := approved
	if err := validateDefinitionDNSCleanup(approved, current); err != nil {
		t.Fatalf("approved cleanup rejected: %v", err)
	}
	current.Changes = nil
	if err := validateDefinitionDNSCleanup(approved, current); err != nil {
		t.Fatalf("already removed approved record rejected: %v", err)
	}
	changed := record
	changed.Content = "1.1.1.1"
	current.Changes = []integrations.DomainDNSChange{{Action: "delete", Type: "A", Before: &changed}}
	if err := validateDefinitionDNSCleanup(approved, current); err == nil {
		t.Fatal("changed owned record accepted")
	}
	changed = record
	changed.ID = "new"
	if err := validateDefinitionDNSCleanup(approved, current); err == nil {
		t.Fatal("new owned record accepted")
	}
	current.Changes = []integrations.DomainDNSChange{change, change}
	if err := validateDefinitionDNSCleanup(approved, current); err == nil {
		t.Fatal("duplicate owned record accepted")
	}
}

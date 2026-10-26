package cli

import (
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

func TestPlatformCommandMetadata(t *testing.T) {
	a := newApp()
	if err := a.meta.validate(a.root); err != nil {
		t.Fatal(err)
	}
	if _, err := a.resolve([]string{"platform"}); err == nil {
		t.Fatal("the user-facing platform command group is still registered")
	}
	for _, path := range [][]string{
		{"app", "create"}, {"app", "list"}, {"app", "status"}, {"app", "delete"},
		{"env", "create"}, {"env", "update"}, {"env", "list"}, {"env", "status"}, {"env", "delete"},
		{"process", "list"}, {"process", "scale"},
		{"config", "set"}, {"config", "unset"}, {"config", "list"},
		{"deploy"}, {"rollback"}, {"restart"},
		{"build", "list"}, {"build", "logs"}, {"build", "stop"}, {"build", "retry"},
		{"release", "list"},
	} {
		cmd, remaining, err := a.root.Find(path)
		if err != nil || len(remaining) != 0 || cmd.RunE == nil {
			t.Fatalf("missing implemented command %s: %v", strings.Join(path, " "), err)
		}
	}
}

func TestRollbackRequiresExactTargetConfirmation(t *testing.T) {
	code, stdout, stderr := invoke(t, "rollback", "api", "production", "--json")
	envelope := decodeOne(t, stdout)
	if code != 5 || stderr != "" || envelope.OK || envelope.Error == nil || envelope.Error.Code != contract.ConfirmationRequired {
		t.Fatalf("missing confirmation: code=%d stderr=%q envelope=%+v", code, stderr, envelope)
	}
	code, stdout, stderr = invoke(t, "rollback", "api", "production", "--confirm", "api/staging", "--json")
	envelope = decodeOne(t, stdout)
	if code != 5 || stderr != "" || envelope.OK || envelope.Error == nil || envelope.Error.Code != contract.PreconditionFailed {
		t.Fatalf("mismatched confirmation: code=%d stderr=%q envelope=%+v", code, stderr, envelope)
	}
}

func TestDeliveryInputMapping(t *testing.T) {
	deploy, err := deployInput("api", "production", " main ", "")
	if err != nil || deploy.App != "api" || deploy.Environment != "production" || deploy.Ref != "main" || deploy.ReleaseID != "" {
		t.Fatalf("source deploy input: %+v %v", deploy, err)
	}
	release := "rel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	deploy, err = deployInput("api", "production", "", release)
	if err != nil || deploy.ReleaseID != "rel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || deploy.Ref != "" {
		t.Fatalf("release deploy input: %+v %v", deploy, err)
	}
	if _, err := deployInput("api", "production", "main", release); err == nil {
		t.Fatal("deploy accepted --ref with --release")
	}
	if _, err := deployInput("api", "production", "", "not-a-release"); err == nil {
		t.Fatal("deploy accepted an invalid release ID")
	}
	rollback, err := rollbackInput("api", "production", release)
	if err != nil || rollback.ToReleaseID != "rel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("rollback input: %+v %v", rollback, err)
	}
	if _, err := rollbackInput("api", "production", "not-a-release"); err == nil {
		t.Fatal("rollback accepted an invalid release ID")
	}
}

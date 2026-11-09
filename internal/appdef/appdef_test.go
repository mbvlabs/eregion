package appdef

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/retention"
)

func ptr[T any](value T) *T { return &value }

func TestParseIsStrictAndVersioned(t *testing.T) {
	valid := []byte("version = 1\napp = \"api\"\n")
	definition, err := Parse(valid)
	if err != nil {
		t.Fatal(err)
	}
	if definition.App != "api" {
		t.Fatalf("definition = %+v", definition)
	}
	for name, raw := range map[string]string{
		"missing version": "app = \"api\"\n",
		"wrong version":   "version = 2\napp = \"api\"\n",
		"unknown key":     "version = 1\nunknown = \"x\"\n",
		"secret values":   "version = 1\n[secrets]\nDATABASE_URL = \"postgres://user:pass@example/db\"\n",
		"duplicate key":   "version = 1\napp = \"one\"\napp = \"two\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(raw))
			if err == nil {
				t.Fatal("accepted invalid definition")
			}
			if strings.Contains(err.Error(), "postgres://user:pass@example/db") {
				t.Fatalf("error echoed a secret value: %v", err)
			}
		})
	}
}

func TestResourceDeclarationValidation(t *testing.T) {
	if _, err := Parse([]byte("version = 1\napp = \"api\"\n[[resources]]\nname = \"db\"\n")); err == nil {
		t.Fatal("accepted a resource without an image or health command")
	}
	definition, err := Parse([]byte(`
version = 1
app = "api"

[[resources]]
name = "db"
image = "postgres:17"
port = 5432

[resources.health]
command = ["pg_isready"]

[resources.secret_env]
POSTGRES_PASSWORD = "DATABASE_PASSWORD"

[[resources.mounts]]
name = "data"
destination = "/var/lib/postgresql/data"
`))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := definition.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	specs, err := resolved.ResourceApplySpecs()
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].Name != "db" || specs[0].SecretEnv["POSTGRES_PASSWORD"] != "DATABASE_PASSWORD" {
		t.Fatalf("specs = %+v", specs)
	}
	names, set := resolved.ResourceAttachments()
	if !set || len(names) != 1 || names[0] != "db" {
		t.Fatalf("attachments = %v set=%v", names, set)
	}
}

func TestResolveMergesSharedFieldsAndSelectedOverride(t *testing.T) {
	definition, err := Parse([]byte(`
version = 1
app = "api"
node = "main"
secret_names = ["DATABASE_URL", "API_KEY"]

[source]
kind = "git"
url = "https://github.com/acme/api"
ref = "main"
builder = "paketobuildpacks/builder-jammy-base"

[source.build_env]
BP_A = "one"
BP_B = "two"

[runtime]
health_type = "http"
health_path = "/healthz"
health_timeout_seconds = 30
release_command = ["./migrate"]

[config]
A = "1"
B = "2"

[[processes]]
name = "web"
kind = "web"
container_port = 8080
replicas = 1

[[domains]]
name = "api.example.com"
primary = true

[environments.staging]
node = "main2"
secret_names = []
processes = []

[environments.staging.source]
ref = "next"

[environments.staging.source.build_env]
BP_B = "override"

[environments.staging.config]
B = "3"
C = "4"
`))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := definition.Resolve("staging")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Node != "main2" || !resolved.NodeSet {
		t.Fatalf("node = %+v", resolved)
	}
	if resolved.Source == nil || resolved.Source.Ref == nil || *resolved.Source.Ref != "next" {
		t.Fatalf("source override = %+v", resolved.Source)
	}
	if resolved.Source.URL == nil || *resolved.Source.URL != "https://github.com/acme/api" {
		t.Fatalf("source URL was not inherited: %+v", resolved.Source)
	}
	if resolved.Source.BuildEnv == nil || (*resolved.Source.BuildEnv)["BP_B"] != "override" || (*resolved.Source.BuildEnv)["BP_A"] != "one" {
		t.Fatalf("build environment = %+v", resolved.Source.BuildEnv)
	}
	if resolved.Config["A"] != "1" || resolved.Config["B"] != "3" || resolved.Config["C"] != "4" {
		t.Fatalf("config merge = %+v", resolved.Config)
	}
	if !resolved.SecretNamesSet || len(resolved.SecretNames) != 0 {
		t.Fatalf("explicit empty secret_names did not clear: %+v", resolved.SecretNames)
	}
	if !resolved.ProcessesSet || len(resolved.Processes) != 0 {
		t.Fatalf("explicit empty processes did not clear: %+v", resolved.Processes)
	}
	if _, err := definition.Resolve("production"); err != nil {
		t.Fatal(err)
	}
	// The command selects the environment; a name without an override gets
	// the shared recipe. This is what makes a PR environment work without
	// editing the file.
	if _, err := definition.Resolve("pr-42"); err != nil {
		t.Fatalf("shared recipe was not reusable for an arbitrary environment: %v", err)
	}
	if _, err := Parse([]byte("version = 1\n")); err != nil {
		t.Fatal(err)
	}
	disagree, err := Parse([]byte("version = 1\n[source]\nkind = \"image\"\nimage = \"ghcr.io/acme/api:1\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := disagree.Resolve("staging"); err != nil {
		t.Fatalf("shared recipe did not apply to a differently named environment: %v", err)
	}
}

func TestDefinitionCollectionLimits(t *testing.T) {
	processes := strings.Builder{}
	processes.WriteString("version = 1\napp = \"api\"\n[source]\nkind = \"image\"\nimage = \"ghcr.io/acme/api:1\"\n")
	for index := 0; index < 33; index++ {
		fmt.Fprintf(&processes, "\n[[processes]]\nname = \"p%d\"\nkind = \"worker\"\ncommand = \"./worker\"\n", index)
	}
	if _, err := Parse([]byte(processes.String())); err != nil {
		t.Fatalf("parse-level process limit failed early: %v", err)
	}
	definition, err := Parse([]byte(processes.String()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := definition.Resolve("production"); err == nil {
		t.Fatal("accepted more than 32 processes")
	}

	names := []string{}
	for index := 0; index < 129; index++ {
		names = append(names, fmt.Sprintf("SECRET_%d", index))
	}
	raw := "version = 1\napp = \"api\"\nsecret_names = [\"" + strings.Join(names, `", "`) + "\"]\n[source]\nkind = \"image\"\nimage = \"ghcr.io/acme/api:1\"\n"
	definition, err = Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := definition.Resolve("production"); err == nil {
		t.Fatal("accepted more than 128 secret names")
	}
}

func TestResolveDistinguishesAbsentFromEmpty(t *testing.T) {
	absent, err := Parse([]byte("version = 1\napp = \"api\"\n[source]\nkind = \"image\"\nimage = \"ghcr.io/acme/api:1\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := absent.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ProcessesSet || resolved.ConfigSet || resolved.SecretNamesSet || resolved.DomainsSet {
		t.Fatalf("absent collections were reported as set: %+v", resolved)
	}
	empty, err := Parse([]byte(`version = 1
app = "api"
secret_names = []
processes = []
domains = []
[source]
kind = "image"
image = "ghcr.io/acme/api:1"
[config]
`))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = empty.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.SecretNamesSet || !resolved.ProcessesSet || !resolved.DomainsSet || !resolved.ConfigSet {
		t.Fatalf("explicit empty collections were not reported as set: %+v", resolved)
	}
	if len(resolved.Processes) != 0 || len(resolved.SecretNames) != 0 || len(resolved.Domains) != 0 || len(resolved.Config) != 0 {
		t.Fatalf("explicit empty collections are not empty: %+v", resolved)
	}
}

func TestCreateSpecMapsTheSupportedDefinition(t *testing.T) {
	raw := []byte(`
version = 1
app = "api"
node = "main"
build_node = "builder"
secret_names = ["DATABASE_URL"]

[source]
kind = "git"
url = "https://github.com/acme/api"
ref = "main"
credential_secret = "GIT_READ_TOKEN"
context_path = "apps/api"
builder = "paketobuildpacks/builder-jammy-base"

[source.build_env]
BP_X = "y"

[runtime]
health_type = "tcp"
health_path = "/readyz"
health_timeout_seconds = 45
release_command = ["./migrate", "--yes"]

[config]
APP_ENV = "production"

[[processes]]
name = "web"
kind = "web"
command = "./api"
args = ["serve"]
container_port = 3000
replicas = 2

[[domains]]
name = "API.Example.com."
primary = true
check_path = "/custom"
`)
	definition, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := definition.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := resolved.CreateSpec([]string{"203.0.113.7"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "production" || spec.Node != "main" || spec.BuildNode != "builder" {
		t.Fatalf("identity = %+v", spec)
	}
	if spec.Source.Kind != "git" || spec.Source.URL != "https://github.com/acme/api" ||
		spec.Source.Ref != "main" || spec.Source.CredentialSecretName != "GIT_READ_TOKEN" ||
		spec.Source.ContextPath != "apps/api" || spec.Source.BuildEnv["BP_X"] != "y" {
		t.Fatalf("source = %+v", spec.Source)
	}
	if spec.Runtime.HealthType != "tcp" || spec.Runtime.HealthPath != "/readyz" ||
		spec.Runtime.HealthTimeoutSeconds != 45 || spec.Runtime.ReleaseCommand == nil ||
		strings.Join(spec.Runtime.ReleaseCommand.Argv, " ") != "./migrate --yes" {
		t.Fatalf("runtime = %+v", spec.Runtime)
	}
	if spec.ContainerPort != 3000 || len(spec.Processes) != 1 || spec.Processes[0].Replicas != 2 {
		t.Fatalf("processes = %+v port=%d", spec.Processes, spec.ContainerPort)
	}
	if spec.Config["APP_ENV"] != "production" || len(spec.SecretNames) != 1 || spec.SecretNames[0] != "DATABASE_URL" {
		t.Fatalf("config/secrets = %+v %+v", spec.Config, spec.SecretNames)
	}
	if len(spec.Domains) != 1 || spec.Domains[0].Domain != "api.example.com" ||
		!spec.Domains[0].Primary || spec.Domains[0].CheckPath != "/custom" {
		t.Fatalf("domains = %+v", spec.Domains)
	}
	if len(spec.DomainAddresses) != 1 {
		t.Fatalf("domain addresses = %v", spec.DomainAddresses)
	}
}

func TestCreateSpecRefusesUnrepresentableCollections(t *testing.T) {
	base := `version = 1
app = "api"
[source]
kind = "image"
image = "ghcr.io/acme/api:1"
`
	for name, extra := range map[string]string{
		"empty processes":    "",
		"domain not primary": "[[domains]]\nname = \"a.example.com\"\n",
		"credential in app":  "",
	} {
		t.Run(name, func(t *testing.T) {
			raw := base + extra
			switch name {
			case "empty processes":
				raw = "version = 1\napp = \"api\"\nprocesses = []\n" + base[len("version = 1\napp = \"api\"\n"):]
			case "credential in app":
				raw = `version = 1
app = "api"
secret_names = ["GIT_READ_TOKEN"]
[source]
kind = "git"
url = "https://github.com/acme/api"
credential_secret = "GIT_READ_TOKEN"
`
			}
			definition, err := Parse([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := definition.Resolve("production")
			if err != nil {
				return
			}
			if _, err := resolved.CreateSpec(nil); err == nil {
				t.Fatal("accepted an unrepresentable create definition")
			}
		})
	}
}

func TestCreateSpecCarriesMultipleDomains(t *testing.T) {
	raw := []byte(`
version = 1
app = "api"
[source]
kind = "image"
image = "ghcr.io/acme/api:1"
[[domains]]
name = "api.example.com"
primary = true
check_path = "/healthz"
[[domains]]
name = "www.example.com"
check_path = "/readyz"
`)
	definition, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := definition.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := resolved.CreateSpec([]string{"203.0.113.7"})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Domains) != 2 || spec.Domains[0].Domain != "api.example.com" || !spec.Domains[0].Primary ||
		spec.Domains[0].CheckPath != "/healthz" || spec.Domains[1].Domain != "www.example.com" ||
		spec.Domains[1].Primary || spec.Domains[1].CheckPath != "/readyz" {
		t.Fatalf("domains = %+v", spec.Domains)
	}
	if len(spec.DomainAddresses) != 1 || spec.DomainAddresses[0] != "203.0.113.7" {
		t.Fatalf("domain addresses = %v", spec.DomainAddresses)
	}
	if spec.Domain != "" || spec.DomainPrimary || spec.DomainCheckPath != "" {
		t.Fatalf("single-domain fields leaked into a multi-domain declaration: %+v", spec)
	}
}

func TestUpdatePatchChangesOnlySupportedFields(t *testing.T) {
	raw := []byte(`
version = 1
app = "api"
node = "main2"
secret_names = []
processes = []
domains = []

[source]
kind = "git"
url = "https://github.com/acme/api"
ref = "next"
credential_secret = "GIT_READ_TOKEN"
builder = "paketobuildpacks/builder-jammy-base"

[runtime]
health_path = "/readyz"

[config]
B = "3"
`)
	definition, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := definition.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	keep := 7
	revisionID := platform.RevisionID("erv_cccccccccccccccccccccccccccccccc")
	current := platform.Environment{
		App: "api", Name: "production", Node: "main", BuildNode: "builder", RevisionID: revisionID,
		Revision: &platform.EnvironmentRevision{ID: revisionID, RevisionState: platform.RevisionState{
			Source: platform.SourceBinding{
				Kind: "git", URL: "https://github.com/acme/api", Ref: "main",
				CredentialSecretName: "GIT_READ_TOKEN", Builder: platform.DefaultBuilder,
			},
			AutoDeploy:        true,
			WebhookSecretName: "DEPLOY_HOOK",
			Runtime: platform.RuntimeSpec{
				Node: "main", HealthType: "http", HealthPath: "/healthz", HealthTimeoutSeconds: 30,
				ReleaseCommand: &platform.CommandSpec{Argv: []string{"./migrate"}},
			},
			Processes: []platform.ProcessSpec{
				{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080},
				{Name: "worker", Kind: "worker", Command: "./worker", Replicas: 1},
			},
			Config:      map[string]string{"A": "1", "B": "2"},
			SecretNames: []string{"DATABASE_URL"},
			Retention:   retention.Override{KeepReleases: &keep},
		}},
	}
	patch, changed, diff, err := resolved.UpdatePatch(current, nil)
	if err != nil {
		t.Fatal(err)
	}
	if patch.Node == nil || *patch.Node != "main2" {
		t.Fatalf("node patch = %+v", patch.Node)
	}
	if patch.BuildNode == nil || *patch.BuildNode != "" {
		t.Fatalf("build node patch = %+v", patch.BuildNode)
	}
	if patch.Source == nil || patch.Source.Ref != "next" || patch.Source.URL != "https://github.com/acme/api" {
		t.Fatalf("source patch = %+v", patch.Source)
	}
	if patch.Runtime == nil || patch.Runtime.HealthPath != "/readyz" ||
		patch.Runtime.HealthType != "http" || patch.Runtime.HealthTimeoutSeconds != 30 ||
		patch.Runtime.ReleaseCommand != nil {
		t.Fatalf("runtime patch = %+v", patch.Runtime)
	}
	if patch.Processes == nil || len(*patch.Processes) != 0 {
		t.Fatalf("process patch = %+v", patch.Processes)
	}
	if patch.Config == nil || len(*patch.Config) != 1 || (*patch.Config)["B"] != "3" {
		t.Fatalf("config patch = %+v", patch.Config)
	}
	if patch.SecretNames == nil || len(*patch.SecretNames) != 0 {
		t.Fatalf("secret patch = %+v", patch.SecretNames)
	}
	if patch.AutoDeploy != nil || patch.WebhookSecretName != nil || patch.Retention != nil {
		t.Fatalf("unsupported/unchanged server-managed state was patched: %+v", patch)
	}
	if patch.ExpectedRevisionID != revisionID {
		t.Fatalf("expected revision fence = %q, want %q", patch.ExpectedRevisionID, revisionID)
	}
	if got := strings.Join(changed, ","); got != "node,build_node,source,runtime,processes,config,secret_names" {
		t.Fatalf("changed = %s", got)
	}
	if len(diff) == 0 {
		t.Fatal("no human-readable diff was produced")
	}
}

func TestUpdatePatchRejectsSourceCredentialRoleAliasing(t *testing.T) {
	current := platform.Environment{
		App: "api", Name: "production", Node: "main",
		Revision: &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
			Source: platform.SourceBinding{
				Kind: "git", URL: "https://github.com/acme/api", Ref: "main",
				CredentialSecretName: "GIT_READ_TOKEN", Builder: platform.DefaultBuilder,
			},
			AutoDeploy:        true,
			WebhookSecretName: "EREGION_WEBHOOK_SECRET",
			Runtime:           platform.RuntimeSpec{Node: "main", HealthType: "http", HealthPath: "/", HealthTimeoutSeconds: 30},
			Processes: []platform.ProcessSpec{
				{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080},
			},
			Config:      map[string]string{},
			SecretNames: []string{"DATABASE_URL"},
		}},
	}
	for _, test := range []struct {
		name        string
		credential  string
		secretNames string
		wantError   string
	}{
		{name: "runtime secret", credential: "GIT_READ_TOKEN", secretNames: `["GIT_READ_TOKEN"]`, wantError: "runtime secret"},
		{name: "preserved webhook", credential: "EREGION_WEBHOOK_SECRET", secretNames: `[]`, wantError: "webhook secret"},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := fmt.Sprintf(`
version = 1
app = "api"
node = "main"
secret_names = %s

[source]
kind = "git"
url = "https://github.com/acme/api"
ref = "main"
credential_secret = "%s"

[runtime]
health_path = "/"

[[processes]]
name = "web"
kind = "web"
container_port = 8080
replicas = 1
`, test.secretNames, test.credential)
			definition, err := Parse([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := definition.Resolve("production")
			if err != nil {
				t.Fatal(err)
			}
			fault := contract.Classify(func() error {
				_, _, _, err := resolved.UpdatePatch(current, nil)
				return err
			}())
			if fault.Code != contract.ValidationFailed || !strings.Contains(fault.Hint, test.wantError) {
				t.Fatalf("UpdatePatch error = %+v", fault)
			}
		})
	}
}

func TestUpdatePatchClearsExplicitEmptyCollections(t *testing.T) {
	raw := []byte(`
version = 1
app = "api"
node = "main"
secret_names = []
resources = []

[source]
kind = "git"
url = "https://github.com/acme/api"
ref = ""
builder = "paketobuildpacks/builder-jammy-base"

[runtime]
release_command = []
`)
	definition, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := definition.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	current := platform.Environment{
		App: "api", Name: "production", Node: "main", BuildNode: "builder",
		Revision: &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
			Source: platform.SourceBinding{
				Kind: "git", URL: "https://github.com/acme/api", Ref: "main",
				Builder: platform.DefaultBuilder, BuildEnv: map[string]string{"BP": "x"},
			},
			Runtime: platform.RuntimeSpec{
				Node: "main", HealthType: "http", HealthPath: "/", HealthTimeoutSeconds: 30,
				ReleaseCommand: &platform.CommandSpec{Argv: []string{"./migrate"}},
			},
			Processes:   []platform.ProcessSpec{{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080}},
			Config:      map[string]string{"A": "1"},
			SecretNames: []string{"DATABASE_URL"},
			Resources:   []string{"db"},
		}},
	}
	patch, changed, _, err := resolved.UpdatePatch(current, nil)
	if err != nil {
		t.Fatal(err)
	}
	if patch.BuildNode == nil || *patch.BuildNode != "" {
		t.Fatalf("build node was not explicitly cleared: %+v", patch.BuildNode)
	}
	if patch.Source == nil || len(patch.Source.BuildEnv) != 0 {
		t.Fatalf("build environment was not cleared: %+v", patch.Source)
	}
	if patch.Runtime == nil || patch.Runtime.ReleaseCommand != nil {
		t.Fatalf("release command was not cleared: %+v", patch.Runtime)
	}
	if patch.Processes == nil || len(*patch.Processes) != 0 {
		t.Fatalf("processes were not cleared: %+v", patch.Processes)
	}
	if patch.Config == nil || len(*patch.Config) != 0 {
		t.Fatalf("config was not cleared: %+v", patch.Config)
	}
	if patch.SecretNames == nil || len(*patch.SecretNames) != 0 {
		t.Fatalf("secret names were not cleared: %+v", patch.SecretNames)
	}
	if patch.Resources == nil || len(*patch.Resources) != 0 {
		t.Fatalf("resources were not cleared: %+v", patch.Resources)
	}
	for _, want := range []string{"build_node", "source", "runtime", "processes", "config", "secret_names", "resources"} {
		if !contains(changed, want) {
			t.Fatalf("changed %v does not name %s", changed, want)
		}
	}
	wire, err := json.Marshal(patch)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(wire, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"processes", "secret_names", "resources"} {
		if got := string(fields[name]); got != "[]" {
			t.Fatalf("%s wire value = %s, want an explicit empty array; patch = %s", name, got, wire)
		}
	}
	if got := string(fields["config"]); got != "{}" {
		t.Fatalf("config wire value = %s, want an explicit empty object; patch = %s", got, wire)
	}
}

func TestUpdatePatchRequiresNodeAndSource(t *testing.T) {
	base := "version = 1\napp = \"api\"\n"
	for name, raw := range map[string]string{
		"missing node":   base + "[source]\nkind = \"image\"\nimage = \"ghcr.io/acme/api:1\"\n",
		"missing source": base + "node = \"main\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			definition, err := Parse([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := definition.Resolve("production")
			if err != nil {
				t.Fatal(err)
			}
			current := platform.Environment{App: "api", Name: "production", Node: "main"}
			if _, _, _, err := resolved.UpdatePatch(current, nil); err == nil {
				t.Fatal("accepted an incomplete update definition")
			}
		})
	}
}

func TestUpdatePatchDetectsAnOmittedDomainRemoval(t *testing.T) {
	raw := []byte(`
version = 1
app = "api"
node = "main"
[source]
kind = "image"
image = "ghcr.io/acme/api:1"
`)
	definition, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := definition.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	current := platform.Environment{App: "api", Name: "production", Node: "main",
		Revision: &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
			Source: platform.SourceBinding{Kind: "image", Image: "ghcr.io/acme/api:1"},
		}},
	}
	if _, _, _, err := resolved.UpdatePatch(current, []platform.DomainBinding{
		{Domain: "api.example.com", Primary: true},
	}); err != nil {
		t.Fatalf("revision update should not own domain changes: %v", err)
	}
	if len(resolved.Domains) != 0 {
		t.Fatalf("omitted domains did not resolve to an explicit removal: %+v", resolved.Domains)
	}
}

func TestUpdatePatchLeavesDomainChangesToTheDomainPlan(t *testing.T) {
	raw := []byte(`
version = 1
app = "api"
node = "main"
[source]
kind = "image"
image = "ghcr.io/acme/api:1"
[[domains]]
name = "api.example.com"
primary = true
`)
	definition, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := definition.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	current := platform.Environment{App: "api", Name: "production",
		Revision: &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
			Source: platform.SourceBinding{Kind: "image", Image: "ghcr.io/acme/api:1"},
		}},
	}
	if _, _, _, err := resolved.UpdatePatch(current, []platform.DomainBinding{{Domain: "other.example.com", Primary: true}}); err != nil {
		t.Fatalf("revision patch should not decide domain drift: %v", err)
	}
}

func TestExportIsDeterministicAndRoundTrips(t *testing.T) {
	revision := &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
		Source: platform.SourceBinding{
			Kind: "git", URL: "https://github.com/acme/api", Ref: "main",
			CredentialSecretName: "GIT_READ_TOKEN", Builder: platform.DefaultBuilder,
			BuildEnv: map[string]string{"BP_Z": "z", "BP_A": "a"},
		},
		Runtime: platform.RuntimeSpec{
			Node: "main", HealthType: "http", HealthPath: "/healthz", HealthTimeoutSeconds: 30,
			ReleaseCommand: &platform.CommandSpec{Argv: []string{"./migrate", "--yes"}},
		},
		Processes: []platform.ProcessSpec{
			{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080},
			{Name: "worker", Kind: "worker", Command: "./worker", Args: []string{"--queue", "default"}, Replicas: 2},
		},
		Config:      map[string]string{"Z": "last", "A": "first"},
		SecretNames: []string{"DATABASE_URL", "API_KEY"},
	}}
	domains := []platform.DomainBinding{
		{Domain: "api.example.com", App: "api", Environment: "production", Primary: true, CheckPath: "/readyz"},
		{Domain: "www.example.com", App: "api", Environment: "production", CheckPath: "/alias"},
	}
	first, err := Export("api", "production", "", revision, domains, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Export("api", "production", "", revision, domains, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("export is not deterministic:\n%s\n---\n%s", first, second)
	}
	if strings.Index(first, `app = "api"`) > strings.Index(first, "[source]") {
		t.Fatalf("root keys are not before tables:\n%s", first)
	}
	if strings.Contains(first, "credential") && strings.Contains(first, "hunter2") {
		t.Fatal("export leaked a credential value")
	}
	definition, err := Parse([]byte(first))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := definition.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	current := platform.Environment{App: "api", Name: "production", Node: "main", BuildNode: "", Revision: revision}
	patch, changed, _, err := resolved.UpdatePatch(current, domains)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Fatalf("round-trip changed fields: %v patch=%+v", changed, patch)
	}
	spec, err := resolved.CreateSpec(nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Source.BuildEnv["BP_A"] != "a" || spec.Config["A"] != "first" {
		t.Fatalf("round-trip lost maps: source=%+v config=%+v", spec.Source, spec.Config)
	}
	if len(spec.Processes) != 2 || spec.Processes[1].Args[1] != "default" {
		t.Fatalf("round-trip lost processes: %+v", spec.Processes)
	}
	if len(spec.Domains) != 2 || spec.Domains[0].Domain != "api.example.com" || !spec.Domains[0].Primary ||
		spec.Domains[0].CheckPath != "/readyz" || spec.Domains[1].Domain != "www.example.com" ||
		spec.Domains[1].CheckPath != "/alias" {
		t.Fatalf("round-trip lost domains: %+v", spec.Domains)
	}
}

func TestExportResourcesRoundTrip(t *testing.T) {
	revision := &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
		Source: platform.SourceBinding{Kind: "image", Image: "ghcr.io/acme/api:1"},
		Runtime: platform.RuntimeSpec{
			Node: "main", HealthType: "http", HealthPath: "/healthz", HealthTimeoutSeconds: 30,
		},
		Processes: []platform.ProcessSpec{{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080}},
		Resources: []string{"db"},
	}}
	resources := []platform.Resource{{
		App: "api", Environment: "production", Name: "db",
		Spec: platform.ResourceSpec{
			Image: "postgres:17", Port: 5432,
			Env:       map[string]string{"POSTGRES_DB": "app"},
			SecretEnv: map[string]string{"POSTGRES_PASSWORD": "DATABASE_PASSWORD"},
			Mounts:    []platform.ResourceMount{{Name: "data", Destination: "/var/lib/postgresql/data"}},
			Command:   []string{"postgres", "-c", "max_connections=100"},
			Health:    platform.ResourceHealthSpec{Type: "exec", Command: []string{"pg_isready"}},
		},
	}}
	document, err := Export("api", "production", "", revision, nil, resources)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[[resources]]", `name = "db"`, `POSTGRES_PASSWORD = "DATABASE_PASSWORD"`, `destination = "/var/lib/postgresql/data"`} {
		if !strings.Contains(document, want) {
			t.Fatalf("export missing %q:\n%s", want, document)
		}
	}
	definition, err := Parse([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := definition.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	specs, err := resolved.ResourceApplySpecs()
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].Name != "db" || specs[0].Image != "postgres:17" || specs[0].Port != 5432 {
		t.Fatalf("round-trip specs = %+v", specs)
	}
	if _, err := Export("api", "production", "", revision, nil, nil); err == nil {
		t.Fatal("exported a revision whose resource definition was unavailable")
	}
}

func TestResourceAttachmentsHonorAttachFalse(t *testing.T) {
	definition, err := Parse([]byte(`
version = 1
app = "api"
[source]
kind = "image"
image = "ghcr.io/acme/api:1"

[[resources]]
name = "db"
image = "postgres:17"
port = 5432

[resources.health]
command = ["pg_isready"]

[[resources]]
name = "cache"
image = "redis:7"
attach = false

[resources.health]
command = ["redis-cli", "ping"]
`))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := definition.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	names, set := resolved.ResourceAttachments()
	if !set {
		t.Fatal("resource declarations were not reported as set")
	}
	if len(names) != 1 || names[0] != "db" {
		t.Fatalf("attachments = %v, want only db", names)
	}
	specs, err := resolved.ResourceApplySpecs()
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 || specs[0].Name != "db" || specs[1].Name != "cache" {
		t.Fatalf("apply specs = %+v", specs)
	}
	created, err := resolved.CreateSpec(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Resources) != 1 || created.Resources[0] != "db" {
		t.Fatalf("first revision resources = %v, want only db", created.Resources)
	}
}

func TestResourceDeclarationRejectsDuplicateMountDestination(t *testing.T) {
	_, err := Parse([]byte(`
version = 1
app = "api"
[source]
kind = "image"
image = "ghcr.io/acme/api:1"

[[resources]]
name = "db"
image = "postgres:17"

[resources.health]
command = ["pg_isready"]

[[resources.mounts]]
name = "data"
destination = "/var/lib/postgresql/data"

[[resources.mounts]]
name = "backup"
destination = "/var/lib/postgresql/data/."
`))
	if err == nil {
		t.Fatal("accepted two mounts with the same container destination")
	}
	if !strings.Contains(err.Error(), "destination") {
		t.Fatalf("refusal does not name the destination: %v", err)
	}
}

func TestExportIncludesResourcesAndRoundTrips(t *testing.T) {
	revision := &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
		Source: platform.SourceBinding{Kind: "image", Image: "ghcr.io/acme/api:1"},
		Runtime: platform.RuntimeSpec{
			Node: "main", HealthType: "http", HealthPath: "/healthz", HealthTimeoutSeconds: 30,
		},
		Processes:   []platform.ProcessSpec{{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080}},
		Config:      map[string]string{"APP_ENV": "production"},
		SecretNames: []string{"DATABASE_PASSWORD"},
		Resources:   []string{"db"},
	}}
	db := platform.Resource{Name: "db", App: "api", Environment: "production", Spec: platform.ResourceSpec{
		Image: "postgres:17", Port: 5432,
		Env:       map[string]string{"POSTGRES_DB": "app"},
		SecretEnv: map[string]string{"POSTGRES_PASSWORD": "DATABASE_PASSWORD"},
		Mounts:    []platform.ResourceMount{{Name: "data", Destination: "/var/lib/postgresql/data"}},
		Health:    platform.ResourceHealthSpec{Type: "exec", Command: []string{"pg_isready", "-U", "app"}, TimeoutSeconds: 30, IntervalMS: 500},
	}}
	cache := platform.Resource{Name: "cache", App: "api", Environment: "production", Spec: platform.ResourceSpec{
		Image:  "redis:7",
		Health: platform.ResourceHealthSpec{Type: "exec", Command: []string{"redis-cli", "ping"}, TimeoutSeconds: 30, IntervalMS: 500},
	}}
	document, err := Export("api", "production", "", revision, nil, []platform.Resource{db, cache})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name = "cache"`, `name = "db"`, `attach = false`,
		`POSTGRES_PASSWORD = "DATABASE_PASSWORD"`,
		`destination = "/var/lib/postgresql/data"`,
	} {
		if !strings.Contains(document, want) {
			t.Fatalf("export missing %q:\n%s", want, document)
		}
	}
	definition, err := Parse([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := definition.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	names, set := resolved.ResourceAttachments()
	if !set || len(names) != 1 || names[0] != "db" {
		t.Fatalf("round-trip attachments = %v set=%v", names, set)
	}
	specs, err := resolved.ResourceApplySpecs()
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("round-trip apply specs = %+v", specs)
	}
	byName := map[string]platform.ResourceApplyInput{}
	for _, spec := range specs {
		byName[spec.Name] = spec
	}
	if byName["db"].SecretEnv["POSTGRES_PASSWORD"] != "DATABASE_PASSWORD" ||
		byName["db"].Env["POSTGRES_DB"] != "app" ||
		len(byName["db"].Mounts) != 1 ||
		byName["cache"].Health.Command[1] != "ping" {
		t.Fatalf("round-trip resource specs = %+v", specs)
	}
	current := platform.Environment{App: "api", Name: "production", Node: "main", Revision: revision}
	patch, changed, _, err := resolved.UpdatePatch(current, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Fatalf("resource export round-trip changed fields: %v patch=%+v", changed, patch)
	}
	if patch.Resources != nil {
		t.Fatalf("resource export round-trip rewrote attachments: %+v", patch.Resources)
	}
	created, err := resolved.CreateSpec(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Resources) != 1 || created.Resources[0] != "db" {
		t.Fatalf("round-trip first revision resources = %v", created.Resources)
	}
}

func TestExportRejectsAttachedResourceWithoutDefinition(t *testing.T) {
	revision := &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
		Source:    platform.SourceBinding{Kind: "image", Image: "ghcr.io/acme/api:1"},
		Resources: []string{"db"},
	}}
	_, err := Export("api", "production", "", revision, nil, nil)
	if err == nil {
		t.Fatal("exported a revision that depends on a missing resource definition")
	}
	if !strings.Contains(err.Error(), "db") || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("refusal does not name the missing resource: %v", err)
	}
}

func TestUpdatePatchRefusesWhenCurrentBindingsAreMissing(t *testing.T) {
	raw := []byte(`
version = 1
app = "api"
node = "main"
[source]
kind = "image"
image = "ghcr.io/acme/api:1"
`)
	definition, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := definition.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	current := platform.Environment{
		App: "api", Name: "production", Node: "main",
		Domains:       []string{"api.example.com"},
		PrimaryDomain: "api.example.com",
		Revision: &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
			Source: platform.SourceBinding{Kind: "image", Image: "ghcr.io/acme/api:1"},
		}},
	}
	if _, _, _, err := resolved.UpdatePatch(current, nil); err == nil {
		t.Fatal("an omitted domain bypassed the environment's current binding")
	} else if !strings.Contains(err.Error(), "current binding") && !strings.Contains(err.Error(), "domains: changing") {
		t.Fatalf("refusal does not explain the current binding: %v", err)
	}
	if _, _, _, err := resolved.UpdatePatch(current, []platform.DomainBinding{
		{Domain: "api.example.com", Primary: true},
	}); err != nil {
		t.Fatalf("consistent current bindings should be accepted: %v", err)
	}
	if _, _, _, err := resolved.UpdatePatch(current, []platform.DomainBinding{
		{Domain: "api.example.com", Primary: true},
		{Domain: "stale.example.com"},
	}); err == nil {
		t.Fatal("a binding outside the environment's domain list was accepted")
	}
}

func TestExportRejectsUnrepresentableState(t *testing.T) {
	base := &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
		Source: platform.SourceBinding{Kind: "image", Image: "ghcr.io/acme/api:1"},
	}}
	keep := 3
	for name, test := range map[string]struct {
		mutate func(*platform.EnvironmentRevision)
		want   string
	}{
		"auto-deploy": {func(revision *platform.EnvironmentRevision) { revision.AutoDeploy = true }, "auto_deploy"},
		"webhook":     {func(revision *platform.EnvironmentRevision) { revision.WebhookSecretName = "HOOK" }, "webhook_secret_name"},
		"retention": {func(revision *platform.EnvironmentRevision) {
			revision.Retention = retention.Override{KeepReleases: &keep}
		}, "retention override"},
		"no source": {func(revision *platform.EnvironmentRevision) { revision.Source = platform.SourceBinding{} }, "no source binding"},
	} {
		t.Run(name, func(t *testing.T) {
			revision := *base
			revision.RevisionState = platform.CloneRevisionState(base.RevisionState)
			test.mutate(&revision)
			_, err := Export("api", "production", "", &revision, nil, nil)
			if err == nil {
				t.Fatal("exported unrepresentable state")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("refusal %q does not name %q", err, test.want)
			}
		})
	}
	if _, err := Export("api", "production", "", nil, nil, nil); err == nil {
		t.Fatal("exported an environment with no revision")
	}
	if _, err := Export("api", "production", "", base, []platform.DomainBinding{
		{Domain: "a.example.com", Primary: true}, {Domain: "b.example.com", Primary: true},
	}, nil); err == nil {
		t.Fatal("exported multiple primary domains")
	}
}

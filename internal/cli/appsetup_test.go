package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/spf13/cobra"
)

func TestAppCreateFlagsBuildTheFirstRevision(t *testing.T) {
	credentialFile := writeSecretInput(t, "read-token")
	secretFile := writeSecretInput(t, "db-secret")

	cmd := &cobra.Command{Use: "create"}
	cmd.Flags().String("name", "", "app")
	cmd.Flags().String("environment", "production", "environment")
	cmd.Flags().Bool("no-deploy", false, "no deploy")
	addSetupFlags(cmd, true)
	args := []string{
		"--name", "api",
		"--environment", "production",
		"--node", "main",
		"--git-url", "https://github.com/acme/api",
		"--git-ref", "main",
		"--git-credential-file", credentialFile,
		"--container-port", "3000",
		"--health-path", "/healthz",
		"--process", "web=web:3000:1:",
		"--process", "worker=worker::2:./worker --queue default",
		"--config", "APP_ENV=production",
		"--config", "LOG_LEVEL=info",
		"--secret", "DATABASE_URL=" + secretFile,
		"--domain", "api.example.com",
		"--no-deploy",
	}
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	a := &app{secrets: &secrets.Registry{}}
	ans, err := a.readSetup(cmd, setupApp)
	if err != nil {
		t.Fatal(err)
	}
	defer ans.clearSecrets()

	if ans.App != "api" || ans.Environment != "production" || ans.Node != "main" || ans.Deploy {
		t.Fatalf("identity/deploy mismatch: %+v", ans)
	}
	if ans.GitURL == "" || ans.GitURL != "https://github.com/acme/api" || ans.GitRef != "main" {
		t.Fatalf("source mismatch: %+v", ans)
	}
	if ans.CredentialName != "GIT_READ_TOKEN" || string(ans.CredentialValue) != "read-token" {
		t.Fatalf("credential mismatch: name=%q value=%q", ans.CredentialName, ans.CredentialValue)
	}
	if len(ans.Processes) != 2 {
		t.Fatalf("processes = %+v", ans.Processes)
	}
	if ans.Processes[0].Name != "web" || ans.Processes[0].ContainerPort != 3000 || ans.Processes[0].Replicas != 1 {
		t.Fatalf("web process = %+v", ans.Processes[0])
	}
	worker := ans.Processes[1]
	if worker.Name != "worker" || worker.Kind != "worker" || worker.Replicas != 2 ||
		worker.Command != "./worker" || strings.Join(worker.Args, " ") != "--queue default" {
		t.Fatalf("worker process = %+v", worker)
	}
	if ans.Config["APP_ENV"] != "production" || ans.Config["LOG_LEVEL"] != "info" {
		t.Fatalf("config = %+v", ans.Config)
	}
	if string(ans.SecretValues["DATABASE_URL"]) != "db-secret" {
		t.Fatalf("secret values = %+v", ans.SecretValues)
	}
	if ans.Domain != "api.example.com" {
		t.Fatalf("domain = %q", ans.Domain)
	}

	spec, err := ans.environmentSpec(nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Source.CredentialSecretName != "GIT_READ_TOKEN" || spec.Source.URL != ans.GitURL {
		t.Fatalf("spec source = %+v", spec.Source)
	}
	if len(spec.SecretNames) != 1 || spec.SecretNames[0] != "DATABASE_URL" {
		t.Fatalf("secret names = %+v", spec.SecretNames)
	}
	if spec.ContainerPort != 3000 || spec.Health.Path != "/healthz" || len(spec.Processes) != 2 {
		t.Fatalf("spec runtime/processes = %+v", spec)
	}
}

func TestAppCreateGitLessDefaultsToNoFirstDeploy(t *testing.T) {
	cmd := &cobra.Command{Use: "create"}
	cmd.Flags().String("name", "", "app")
	cmd.Flags().String("image-repository", "", "repository")
	cmd.Flags().Bool("no-deploy", false, "no deploy")
	addSetupFlags(cmd, true)
	if err := cmd.ParseFlags([]string{"--name", "api", "--node", "main", "--image-repository", "mbvlabs/api"}); err != nil {
		t.Fatal(err)
	}
	a := &app{secrets: &secrets.Registry{}}
	ans, err := a.readSetup(cmd, setupApp)
	if err != nil {
		t.Fatal(err)
	}
	defer ans.clearSecrets()
	if ans.Deploy {
		t.Fatal("a Git-less app create requested a first deploy without an explicit selector")
	}
	spec, err := ans.environmentSpec(nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Source.URL != "" {
		t.Fatalf("Git-less app create carried a source: %+v", spec.Source)
	}
}

func TestAppCreateGitLessFromFileDefaultsToNoFirstDeploy(t *testing.T) {
	definition := writeDefinitionFile(t, "version = 1\napp = \"api\"\nnode = \"main\"\nimage_repository = \"mbvlabs/api\"\n")
	cmd := &cobra.Command{Use: "create"}
	cmd.Flags().String("name", "", "app")
	cmd.Flags().String("image-repository", "", "repository")
	cmd.Flags().Bool("no-deploy", false, "no deploy")
	addSetupFlags(cmd, true)
	if err := cmd.ParseFlags([]string{"--from", definition}); err != nil {
		t.Fatal(err)
	}
	a := &app{secrets: &secrets.Registry{}}
	ans, err := a.readSetup(cmd, setupApp)
	if err != nil {
		t.Fatal(err)
	}
	defer ans.clearSecrets()
	if ans.Deploy {
		t.Fatal("a Git-less file app create requested a first deploy without an explicit selector")
	}
}

func TestAppCreateImageOnlyWithoutNoDeploySubmitsNoDeploy(t *testing.T) {
	backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{}}
	a := newApp()
	a.backend = backend
	var stdout, stderr bytes.Buffer
	exit := runApp(context.Background(), []string{
		"app", "create", "--name", "api", "--node", "main",
		"--image-repository", "mbvlabs/api", "--detach", "--json",
	}, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	submissions := backend.submissionsOfKind(platform.KindAppCreate)
	var actual *definitionSubmission
	for index := range submissions {
		if !submissions[index].DryRun {
			actual = &submissions[index]
		}
	}
	if actual == nil {
		t.Fatalf("no accepted app create submission: %+v", submissions)
	}
	var input platform.AppCreateInput
	if err := json.Unmarshal(actual.Input, &input); err != nil {
		t.Fatal(err)
	}
	if input.Deploy {
		t.Fatal("image-only app create implicitly requested a first deploy")
	}
}

func TestAppCreateBuildsProtectedProductionAndStagingAtomically(t *testing.T) {
	backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{}}
	a := newApp()
	a.backend = backend
	var stdout, stderr bytes.Buffer
	exit := runApp(context.Background(), []string{
		"app", "create", "--name", "api", "--node", "main",
		"--image-repository", "mbvlabs/api", "--domain", "api.example.com", "--no-deploy", "--detach", "--json",
	}, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	var input platform.AppCreateInput
	found := false
	for _, submission := range backend.submissionsOfKind(platform.KindAppCreate) {
		if submission.DryRun {
			continue
		}
		if err := json.Unmarshal(submission.Input, &input); err != nil {
			t.Fatal(err)
		}
		found = true
	}
	if !found {
		t.Fatalf("app create submissions = %+v", backend.submissions)
	}
	if len(input.Environments) != 2 || input.Environments[0].Name != "production" || input.Environments[1].Name != "staging" {
		t.Fatalf("environments = %+v", input.Environments)
	}
	for _, spec := range input.Environments {
		if spec.Protected == nil || !*spec.Protected {
			t.Fatalf("%s was not protected by default: %+v", spec.Name, spec.Protected)
		}
	}
	if len(input.Environments[0].Domains) != 1 || input.Environments[0].Domains[0].Domain != "api.example.com" {
		t.Fatalf("production domains = %+v", input.Environments[0].Domains)
	}
	if input.Environments[1].Domain != "" || len(input.Environments[1].Domains) != 0 {
		t.Fatalf("staging invented a domain: %+v", input.Environments[1])
	}
}

func TestAppCreateWritesSharedSecretsToBothEnvironmentNamespaces(t *testing.T) {
	credentialFile := writeSecretInput(t, "read-token")
	secretFile := writeSecretInput(t, "db-secret")
	backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{}}
	a := newApp()
	a.backend = backend
	var stdout, stderr bytes.Buffer
	exit := runApp(context.Background(), []string{
		"app", "create", "--name", "api", "--node", "main",
		"--git-url", "https://github.com/acme/api",
		"--git-credential-file", credentialFile,
		"--secret", "DATABASE_URL=" + secretFile,
		"--no-deploy", "--json",
	}, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	want := map[string]string{
		"api/production/GIT_READ_TOKEN": "read-token",
		"api/staging/GIT_READ_TOKEN":    "read-token",
		"api/production/DATABASE_URL":   "db-secret",
		"api/staging/DATABASE_URL":      "db-secret",
	}
	if len(backend.secretNamespaces) != len(want) {
		t.Fatalf("secret writes = %+v, want %d namespaces", backend.secretNamespaces, len(want))
	}
	for namespace, value := range want {
		if got := backend.secretPayloads[namespace]; got != value {
			t.Fatalf("secret %s = %q, want %q; all writes=%+v", namespace, got, value, backend.secretPayloads)
		}
	}
}

func TestSetupSecretWritesRespectEnvironmentSpecificNames(t *testing.T) {
	backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{}}
	a := &app{secrets: &secrets.Registry{}, backend: backend}
	ans := &setupAnswers{
		App:             "api",
		CredentialName:  "PROD_GIT",
		CredentialValue: []byte("prod-token"),
		SecretValues:    map[string][]byte{"PROD_ONLY": []byte("prod"), "STAGE_ONLY": []byte("stage")},
	}
	prod := platform.EnvironmentSpec{
		Name: "production", SecretNames: []string{"PROD_ONLY"},
		Source: platform.SourceBinding{URL: "https://github.com/acme/api", CredentialSecretName: "PROD_GIT"},
	}
	stage := platform.EnvironmentSpec{
		Name: "staging", SecretNames: []string{"STAGE_ONLY"},
		Source: platform.SourceBinding{URL: "https://github.com/acme/api", CredentialSecretName: "STAGE_GIT"},
	}
	if _, err := a.writeSetupSecretsForEnvironments(&cobra.Command{}, ans, []platform.EnvironmentSpec{prod, stage}); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"api/production/PROD_ONLY": "prod",
		"api/staging/STAGE_ONLY":   "stage",
		"api/production/PROD_GIT":  "prod-token",
	}
	if len(backend.secretNamespaces) != len(want) {
		t.Fatalf("secret writes = %+v, want only %v", backend.secretNamespaces, want)
	}
	for namespace, value := range want {
		if got := backend.secretPayloads[namespace]; got != value {
			t.Fatalf("secret %s = %q, want %q", namespace, got, value)
		}
	}
	for _, leaked := range []string{
		"api/production/STAGE_ONLY", "api/staging/PROD_ONLY",
		"api/staging/PROD_GIT", "api/production/STAGE_GIT",
	} {
		if _, found := backend.secretPayloads[leaked]; found {
			t.Fatalf("secret leaked into %s", leaked)
		}
	}
}

func TestSetupProtectionFlag(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		{name: "default", want: "default"},
		{name: "protected", args: []string{"--protected"}, want: "protected"},
		{name: "unprotected", args: []string{"--unprotected"}, want: "unprotected"},
		{name: "both", args: []string{"--protected", "--unprotected"}, wantErr: true},
		{name: "both reversed", args: []string{"--unprotected", "--protected"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{Use: "create"}
			addProtectionFlags(cmd)
			if err := cmd.ParseFlags(tc.args); err != nil {
				t.Fatal(err)
			}
			got, err := setupProtectionFlag(cmd)
			if tc.wantErr {
				if err == nil || contract.Classify(err).Code != contract.ValidationFailed {
					t.Fatalf("err = %v, want validation_failed", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch tc.want {
			case "default":
				if got != nil {
					t.Fatalf("default protection = %v, want nil", *got)
				}
			case "protected":
				if got == nil || !*got {
					t.Fatalf("protected = %v, want true", got)
				}
			case "unprotected":
				if got == nil || *got {
					t.Fatalf("unprotected = %v, want false", got)
				}
			}
		})
	}
}

func TestCreateRejectsConflictingProtectionFlags(t *testing.T) {
	definition := writeDefinitionFile(t, testDefinition)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "app create", args: []string{
			"app", "create", "--name", "api",
			"--protected", "--unprotected", "--json",
		}},
		{name: "env create", args: []string{
			"env", "create", "--app", "api", "--name", "production",
			"--unprotected", "--protected", "--json",
		}},
		{name: "app create from file", args: []string{
			"app", "create", "--from", definition, "--name", "api", "--protected", "--unprotected", "--json",
		}},
		{name: "env create from file", args: []string{
			"env", "create", "--from", definition, "--app", "api", "--name", "pr-42", "--unprotected", "--protected", "--json",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{}}
			a := newApp()
			a.backend = backend
			var stdout, stderr bytes.Buffer
			exit := runApp(context.Background(), tc.args, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
			if exit != 2 || stderr.Len() != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
			}
			envelope := decodeOne(t, stdout.String())
			if envelope.OK || envelope.Error == nil || envelope.Error.Code != contract.ValidationFailed ||
				!strings.Contains(envelope.Error.Message, "choose one protection state") {
				t.Fatalf("envelope = %+v", envelope)
			}
			if len(backend.submissions) != 0 {
				t.Fatalf("conflicting flags submitted work: %+v", backend.submissions)
			}
		})
	}
}

func TestEnvProtectionRequiresFactorOnlyToLowerProtection(t *testing.T) {
	environments := func(protected bool) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"items": []platform.Environment{{
			App: "api", Name: "pr-42", Node: "main", Protected: protected,
		}}})
		return raw
	}
	for _, tc := range []struct {
		name      string
		args      []string
		protected bool
		wantExit  int
		wantKind  string
	}{
		{name: "protect is agent-safe", args: []string{"env", "protect", "api", "pr-42", "--json"}, protected: false, wantKind: platform.KindEnvironmentProtect},
		{name: "protected unprotect refuses without a terminal", args: []string{"env", "unprotect", "api", "pr-42", "--json"}, protected: true, wantExit: 2},
		{name: "already unprotected is a no-op", args: []string{"env", "unprotect", "api", "pr-42", "--json"}, protected: false, wantKind: platform.KindEnvironmentProtect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{"environments": environments(tc.protected)}}
			a := newApp()
			a.backend = backend
			var stdout, stderr bytes.Buffer
			exit := runApp(context.Background(), tc.args, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
			if exit != tc.wantExit {
				t.Fatalf("exit=%d want=%d stdout=%s stderr=%s", exit, tc.wantExit, stdout.String(), stderr.String())
			}
			if tc.wantKind != "" && len(backend.submissionsOfKind(tc.wantKind)) != 1 {
				t.Fatalf("submissions = %+v", backend.submissions)
			}
			if tc.wantKind == "" && len(backend.submissions) != 0 {
				t.Fatalf("protected unprotect submitted: %+v", backend.submissions)
			}
		})
	}
}

func TestAppCreateImageRepositoryUsesTheDefaultWebProcess(t *testing.T) {
	cmd := &cobra.Command{Use: "create"}
	cmd.Flags().String("name", "", "app")
	cmd.Flags().String("environment", "production", "environment")
	cmd.Flags().String("image-repository", "", "repository")
	cmd.Flags().Bool("no-deploy", false, "no deploy")
	addSetupFlags(cmd, true)
	if err := cmd.ParseFlags([]string{
		"--name", "api",
		"--node", "main",
		"--image-repository", "mbvlabs/api",
		"--no-deploy",
		"--health-path", "/healthz",
	}); err != nil {
		t.Fatal(err)
	}
	a := &app{secrets: &secrets.Registry{}}
	ans, err := a.readSetup(cmd, setupApp)
	if err != nil {
		t.Fatal(err)
	}
	defer ans.clearSecrets()
	spec, err := ans.environmentSpec(nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Source.URL != "" || spec.Source.Builder != "" || spec.Source.CredentialSecretName != "" {
		t.Fatalf("image-only app carried a source: %+v", spec.Source)
	}
	if len(spec.Processes) != 1 || spec.Processes[0].Name != "web" ||
		spec.Processes[0].ContainerPort != platform.DefaultContainerPort || spec.ContainerPort != platform.DefaultContainerPort {
		t.Fatalf("default web process = %+v port=%d", spec.Processes, spec.ContainerPort)
	}
	if spec.Health.Path != "/healthz" || ans.Deploy {
		t.Fatalf("runtime/deploy = %+v deploy=%t", spec.Health, ans.Deploy)
	}
}

func TestEnvCreateDeployFlagMapping(t *testing.T) {
	build := func(args ...string) *setupAnswers {
		t.Helper()
		cmd := &cobra.Command{Use: "create"}
		cmd.Flags().String("app", "", "app")
		cmd.Flags().String("name", "", "environment")
		cmd.Flags().Bool("no-deploy", false, "no deploy")
		addSetupFlags(cmd, true)
		if err := cmd.ParseFlags(args); err != nil {
			t.Fatal(err)
		}
		a := &app{secrets: &secrets.Registry{}}
		ans, err := a.readSetup(cmd, setupEnvironment)
		if err != nil {
			t.Fatal(err)
		}
		return ans
	}
	base := []string{"--app", "api", "--name", "staging", "--node", "main", "--git-url", "https://github.com/acme/api"}
	if ans := build(base...); !ans.Deploy {
		t.Fatal("env create did not request the first deploy by default")
	}
	if ans := build(append(base, "--no-deploy")...); ans.Deploy {
		t.Fatal("--no-deploy did not clear the env create deploy intent")
	}
}

func TestEnvUpdateFlagsMergeOverTheCurrentRevision(t *testing.T) {
	cmd := &cobra.Command{Use: "update"}
	addEnvironmentUpdateFlags(cmd)
	if err := cmd.ParseFlags([]string{
		"--node", "main2",
		"--git-ref", "release-2026-10",
		"--health-path", "/readyz",
	}); err != nil {
		t.Fatal(err)
	}
	current := platform.Environment{
		App: "api", Name: "production", Node: "main",
		Health: platform.HealthConfig{Type: "http", Path: "/", TimeoutSeconds: 30},
		Revision: &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
			Source: platform.SourceBinding{
				URL: "https://github.com/acme/api", CredentialSecretName: "GIT_READ_TOKEN", Builder: "paketobuildpacks/builder-jammy-base",
			},
			Runtime: platform.RuntimeSpec{
				Node: "main", HealthType: "http", HealthPath: "/", HealthTimeoutSeconds: 30,
				ReleaseCommand: &platform.CommandSpec{Argv: []string{"./migrate"}},
			},
			Processes: []platform.ProcessSpec{{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080}},
			Config:    map[string]string{"APP_ENV": "production"},
		}},
	}
	a := &app{secrets: &secrets.Registry{}}
	patch, credential, credentialName, changed, err := a.buildEnvironmentUpdate(cmd, current)
	if err != nil {
		t.Fatal(err)
	}
	if len(credential) != 0 || credentialName != "" {
		t.Fatalf("unexpected credential: %q %q", credential, credentialName)
	}
	if patch.Node == nil || *patch.Node != "main2" {
		t.Fatalf("node patch = %+v", patch.Node)
	}
	if patch.Source == nil || patch.Source.URL != "https://github.com/acme/api" ||
		patch.Source.Ref != "release-2026-10" || patch.Source.CredentialSecretName != "GIT_READ_TOKEN" ||
		patch.Source.Builder != "paketobuildpacks/builder-jammy-base" {
		t.Fatalf("source patch = %+v", patch.Source)
	}
	if patch.Runtime == nil || patch.Runtime.Node != "main2" || patch.Runtime.HealthPath != "/readyz" ||
		patch.Runtime.HealthType != "http" || patch.Runtime.HealthTimeoutSeconds != 30 ||
		patch.Runtime.ReleaseCommand == nil || strings.Join(patch.Runtime.ReleaseCommand.Argv, " ") != "./migrate" {
		t.Fatalf("runtime patch = %+v", patch.Runtime)
	}
	if strings.Join(changed, ",") != "node,source,runtime" {
		t.Fatalf("changed = %v", changed)
	}
	if patch.Processes != nil || patch.Config != nil || patch.SecretNames != nil {
		t.Fatalf("untouched fields were sent: %+v", patch)
	}
}

func TestParseAssignmentsAndProcessSpecs(t *testing.T) {
	values, err := parseAssignments([]string{"APP_ENV=production", "EMPTY="}, "--config")
	if err != nil {
		t.Fatal(err)
	}
	if values["APP_ENV"] != "production" || values["EMPTY"] != "" {
		t.Fatalf("assignments = %+v", values)
	}
	for _, bad := range [][]string{{"NO_EQUALS"}, {"1BAD=value"}, {"A=1", "A=2"}} {
		if _, err := parseAssignments(bad, "--config"); err == nil {
			t.Fatalf("accepted invalid assignment %v", bad)
		}
	}

	processes, err := parseProcessSpecs([]string{
		"web=web:8080:1:",
		"worker=worker::2:./worker --queue default",
		"api=service:9000:1:./api --mode service",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(processes) != 3 {
		t.Fatalf("processes = %+v", processes)
	}
	if processes[1].Kind != "worker" || processes[1].ContainerPort != 0 || processes[1].Replicas != 2 ||
		processes[1].Command != "./worker" || strings.Join(processes[1].Args, " ") != "--queue default" {
		t.Fatalf("worker = %+v", processes[1])
	}
	if processes[2].Kind != "service" || processes[2].ContainerPort != 9000 {
		t.Fatalf("service = %+v", processes[2])
	}
	for _, bad := range [][]string{{"web=queue:8080"}, {"web=web:0"}, {"worker=worker::500"}} {
		if _, err := parseProcessSpecs(bad); err == nil {
			t.Fatalf("accepted invalid process %v", bad)
		}
	}
}

func TestEnvLinesReadsAPastedBlock(t *testing.T) {
	got := envLines("# app\nexport PORT=8080\n\nSITE_NAME=\"Andurel\"\nEMPTY=\n")
	want := []string{"PORT=8080", "SITE_NAME=Andurel", "EMPTY="}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("envLines = %q, want %q", got, want)
	}
}

func TestSetupResultCarriesTheFirstDeployOperationID(t *testing.T) {
	first := contract.OperationID("op_" + strings.Repeat("a", 32))
	summary := setupResult{
		App: "api", Environment: "production", Node: "main",
		Deploy: true, OperationID: "op_" + contract.OperationID(strings.Repeat("b", 32)),
		FirstDeployOperationID: first, created: "app",
	}
	raw, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["first_deploy_operation_id"] != string(first) {
		t.Fatalf("first_deploy_operation_id = %v, want %s", decoded["first_deploy_operation_id"], first)
	}
	plain := summary.plainText()
	if !strings.Contains(plain, "first deploy operation: "+string(first)) ||
		!strings.Contains(plain, "follow: eregion operations follow "+string(first)) {
		t.Fatalf("accepted plain output lacks the first deploy identity:\n%s", plain)
	}
}

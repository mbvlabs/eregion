package cli

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/resources"
	"github.com/spf13/cobra"
)

// resourceMeta mirrors platform client metadata. A resources command never
// opens Docker, PostgreSQL, or the service state directory locally: it reads
// inventory and submits registered platform actions through the authenticated
// API, so the control plane owns execution and history.
func resourceMeta(summary, example string) CommandMeta {
	meta := platformMeta(summary, example)
	meta.Next = []string{"eregion resources list --json", "eregion commands --json"}
	return meta
}

func resourceMutationMeta(summary, example string) CommandMeta {
	meta := resourceMeta(summary, example)
	meta.Mutating, meta.DryRun = true, true
	return meta
}

func resourceDestructiveMeta(summary, example string) CommandMeta {
	meta := resourceMutationMeta(summary, example)
	meta.Destructive = true
	meta.Confirmation = "plan-bound"
	return meta
}

func resourceOneName(_ *cobra.Command, args []string) error {
	if len(args) != 1 {
		return contract.NewFault(contract.InputRequired, "resource name is required", "Supply exactly one application resource name.")
	}
	if strings.TrimSpace(args[0]) != args[0] || args[0] == "" {
		return contract.NewFault(contract.ValidationFailed, "invalid resource name", "Use a stable lowercase resource name without surrounding whitespace.")
	}
	return nil
}

func addResourceFollowFlags(cmd *cobra.Command) {
	addSubmissionFlags(cmd)
	cmd.Flags().Bool("follow", false, "Follow the submitted operation after it is accepted")
	followFlags(cmd)
}

func addResourceDestructiveFlags(cmd *cobra.Command) {
	addResourceFollowFlags(cmd)
	cmd.Flags().String("confirm", "", "Confirmation token returned by --dry-run for this exact resource and input")
}

func addResourceInventoryFlags(cmd *cobra.Command) {
	cmd.Flags().String("node", "", "Filter by application host")
	cmd.Flags().String("state", "", "Filter by observed resource state")
	cmd.Flags().Int("limit", 50, "Maximum items (1–200)")
}

func (a *app) resourceSpecs() []spec {
	return []spec{{
		use:  "resources",
		meta: resourceMeta("Manage PostgreSQL application resources, backups, credentials, and recovery", "eregion resources list --json"),
		flags: func(cmd *cobra.Command) {
			cmd.PersistentFlags().String("api-url", "", "API origin URL (defaults to EREGION_API_URL or loopback)")
		},
		run: func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			a.resourceCreateSpec(),
			a.resourceListSpec(),
			a.resourceStatusSpec(),
			a.resourceCredentialsSpec(),
			a.resourceConnectInfoSpec(),
			a.resourceDestroySpec(),
			a.resourceRecoverSpec(),
			a.resourceBackupsSpec(),
			a.resourceScheduleSpec(),
		},
	}}
}

func (a *app) resourceCreateSpec() spec {
	return spec{
		use:  "create <name>",
		meta: resourceMutationMeta("Create or converge a PostgreSQL application resource on one explicit host", "eregion resources create appdb --host main --database app --username app --dry-run --json"),
		args: resourceOneName,
		flags: func(cmd *cobra.Command) {
			addResourceFollowFlags(cmd)
			cmd.Flags().String("host", "", "Explicit application host; it must be the worker's verified Docker node")
			cmd.Flags().String("version", "17", "Supported PostgreSQL major version")
			cmd.Flags().String("database", "", "Database name created by the image")
			cmd.Flags().String("username", "", "Database username")
			cmd.Flags().String("address", "127.0.0.1", "Literal published address (never 0.0.0.0)")
			cmd.Flags().Int("port", 0, "Published host port; 0 allocates an ephemeral port")
			cmd.Flags().String("volume", "", "Named Docker volume; empty derives one from the resource name")
		},
		run: func(cmd *cobra.Command, args []string) (result, error) {
			spec, err := resourceSpecFromFlags(cmd, args[0])
			if err != nil {
				return result{}, err
			}
			return a.submitResource(cmd, resources.KindResourceCreate, "node/"+spec.Host, resources.ResourceCreateInput{Spec: spec})
		},
	}
}

func (a *app) resourceListSpec() spec {
	return spec{
		use:  "list",
		meta: resourceMeta("List application resources and their observed state", "eregion resources list --node main --json"),
		args: platformReadArgs,
		flags: func(cmd *cobra.Command) {
			addResourceInventoryFlags(cmd)
		},
		run: func(cmd *cobra.Command, _ []string) (result, error) {
			raw, err := a.platformReadInventoryWith(cmd, "resources", resourceInventoryFilters(cmd))
			if err != nil {
				return result{}, err
			}
			return result{data: raw, plain: resourceTable(raw)}, nil
		},
	}
}

func (a *app) resourceStatusSpec() spec {
	return spec{
		use:  "status <name>",
		meta: resourceMeta("Show one resource's secret-free inventory observation", "eregion resources status appdb --node main --json"),
		args: resourceOneName,
		flags: func(cmd *cobra.Command) {
			cmd.Flags().String("node", "", "Explicit application host when the name exists on more than one")
		},
		run: func(cmd *cobra.Command, args []string) (result, error) {
			item, err := a.resourceItem(cmd, args[0])
			if err != nil {
				return result{}, err
			}
			return result{data: item, plain: "Resource " + args[0] + "\n" + platformPretty(item)}, nil
		},
	}
}

func (a *app) resourceCredentialsSpec() spec {
	meta := resourceMutationMeta("Write resource credentials to one new protected file on the application host", "eregion resources credentials appdb --node main --output-file /run/secrets/appdb.json --idempotency-key ops:appdb-credentials --json")
	meta.InputFlags = []string{"output-file"}
	return spec{
		use:  "credentials <name>",
		meta: meta,
		args: resourceOneName,
		flags: func(cmd *cobra.Command) {
			addResourceFollowFlags(cmd)
			cmd.Flags().String("node", "", "Explicit application host when the name exists on more than one")
			cmd.Flags().String("output-file", "", "New absolute 0600 destination file on the application host; contents are never printed")
		},
		run: func(cmd *cobra.Command, args []string) (result, error) {
			destination, _ := cmd.Flags().GetString("output-file")
			if strings.TrimSpace(destination) == "" {
				return result{}, contract.NewFault(contract.InputRequired, "--output-file is required", "Name a new absolute file; credentials are never printed to stdout.")
			}
			spec, err := a.resourceSpec(cmd, args[0])
			if err != nil {
				return result{}, err
			}
			return a.submitResource(cmd, resources.KindResourceCredentials, "node/"+spec.Host, resources.ResourceCredentialsInput{
				Spec: spec, Destination: destination,
			})
		},
	}
}

func (a *app) resourceConnectInfoSpec() spec {
	return spec{
		use:  "connect-info <name>",
		meta: resourceMeta("Show the non-secret endpoint, credential-file reference, and secret-reference guidance", "eregion resources connect-info appdb --node main --json"),
		args: resourceOneName,
		flags: func(cmd *cobra.Command) {
			cmd.Flags().String("node", "", "Explicit application host when the name exists on more than one")
		},
		run: func(cmd *cobra.Command, args []string) (result, error) {
			return a.resourceConnectInfo(cmd, args[0])
		},
	}
}

func (a *app) resourceDestroySpec() spec {
	return spec{
		use:  "destroy <name>",
		meta: resourceDestructiveMeta("Destroy one application resource after exact plan-bound confirmation", "eregion resources destroy appdb --node main --dry-run --json"),
		args: resourceOneName,
		flags: func(cmd *cobra.Command) {
			addResourceDestructiveFlags(cmd)
			cmd.Flags().String("node", "", "Explicit application host when the name exists on more than one")
			cmd.Flags().Bool("remove-volume", false, "Also delete the labelled named volume and protected credentials")
		},
		run: func(cmd *cobra.Command, args []string) (result, error) {
			spec, err := a.resourceSpec(cmd, args[0])
			if err != nil {
				return result{}, err
			}
			removeVolume, _ := cmd.Flags().GetBool("remove-volume")
			input := resources.ResourceDestroyInput{Spec: spec, RemoveVolume: removeVolume, Confirmation: spec.Name}
			return a.submitResourceDestructive(cmd, resources.KindResourceDestroy, "node/"+spec.Host, input)
		},
	}
}

func (a *app) resourceRecoverSpec() spec {
	return spec{
		use:  "recover <name>",
		meta: resourceMutationMeta("Recover an interrupted restore from its durable journal", "eregion resources recover appdb --node main --idempotency-key ops:appdb-recover --json"),
		args: resourceOneName,
		flags: func(cmd *cobra.Command) {
			addResourceFollowFlags(cmd)
			cmd.Flags().String("node", "", "Explicit application host when the name exists on more than one")
		},
		run: func(cmd *cobra.Command, args []string) (result, error) {
			spec, err := a.resourceSpec(cmd, args[0])
			if err != nil {
				return result{}, err
			}
			return a.submitResource(cmd, resources.KindResourceRecover, "node/"+spec.Host, resources.ResourceRecoverInput{Spec: spec})
		},
	}
}

func (a *app) resourceBackupsSpec() spec {
	return spec{
		use:  "backups",
		meta: resourceMeta("Create, verify, list, and restore PostgreSQL logical backups", "eregion resources backups list --json"),
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{
				use:  "create <name>",
				meta: resourceMutationMeta("Create one logical backup archive for a resource", "eregion resources backups create appdb --node main --idempotency-key ops:appdb-backup-1 --json"),
				args: resourceOneName,
				flags: func(cmd *cobra.Command) {
					addResourceFollowFlags(cmd)
					cmd.Flags().String("node", "", "Explicit application host when the name exists on more than one")
					cmd.Flags().String("reason", "manual", "Backup reason: manual, scheduled, or safety")
				},
				run: func(cmd *cobra.Command, args []string) (result, error) {
					spec, err := a.resourceSpec(cmd, args[0])
					if err != nil {
						return result{}, err
					}
					reason, _ := cmd.Flags().GetString("reason")
					return a.submitResource(cmd, resources.KindBackupCreate, "node/"+spec.Host, resources.BackupCreateInput{Spec: spec, Reason: reason})
				},
			},
			{
				use:  "list",
				meta: resourceMeta("List backup archives and verification observations", "eregion resources backups list --node main --json"),
				args: platformReadArgs,
				flags: func(cmd *cobra.Command) {
					cmd.Flags().String("node", "", "Filter by application host")
					cmd.Flags().Int("limit", 50, "Maximum artifacts (1–200)")
				},
				run: func(cmd *cobra.Command, _ []string) (result, error) {
					raw, err := a.platformReadInventoryWith(cmd, "backups", resourceInventoryFilters(cmd))
					if err != nil {
						return result{}, err
					}
					return result{data: raw, plain: backupTable(raw)}, nil
				},
			},
			{
				use:  "verify <name>",
				meta: resourceMutationMeta("Verify a backup archive checksum and table of contents", "eregion resources backups verify appdb --node main --archive appdb-abcd.dump --idempotency-key ops:appdb-verify-1 --json"),
				args: resourceOneName,
				flags: func(cmd *cobra.Command) {
					addResourceFollowFlags(cmd)
					cmd.Flags().String("node", "", "Explicit application host when the name exists on more than one")
					cmd.Flags().String("archive", "", "Archive name or path inside the configured backup directory")
				},
				run: func(cmd *cobra.Command, args []string) (result, error) {
					archive, _ := cmd.Flags().GetString("archive")
					if strings.TrimSpace(archive) == "" {
						return result{}, contract.NewFault(contract.InputRequired, "--archive is required", "List backups and choose an archive inside the configured destination.")
					}
					spec, err := a.resourceSpec(cmd, args[0])
					if err != nil {
						return result{}, err
					}
					return a.submitResource(cmd, resources.KindBackupVerify, "node/"+spec.Host, resources.BackupVerifyInput{Spec: spec, Archive: archive})
				},
			},
			{
				use:  "restore <name>",
				meta: resourceDestructiveMeta("Restore a verified backup with a safety backup and automatic rollback on failure", "eregion resources backups restore appdb --node main --archive appdb-abcd.dump --dry-run --json"),
				args: resourceOneName,
				flags: func(cmd *cobra.Command) {
					addResourceDestructiveFlags(cmd)
					cmd.Flags().String("node", "", "Explicit application host when the name exists on more than one")
					cmd.Flags().String("archive", "", "Verified archive name or path inside the configured backup directory")
				},
				run: func(cmd *cobra.Command, args []string) (result, error) {
					archive, _ := cmd.Flags().GetString("archive")
					if strings.TrimSpace(archive) == "" {
						return result{}, contract.NewFault(contract.InputRequired, "--archive is required", "List backups and choose a verified archive before restoring.")
					}
					spec, err := a.resourceSpec(cmd, args[0])
					if err != nil {
						return result{}, err
					}
					input := resources.BackupRestoreInput{Spec: spec, Archive: archive, Confirmation: spec.Name}
					return a.submitResourceDestructive(cmd, resources.KindBackupRestore, "node/"+spec.Host, input)
				},
			},
		},
	}
}

func (a *app) resourceScheduleSpec() spec {
	return spec{
		use:  "schedule",
		meta: resourceMeta("Record a backup schedule and retention policy declaration", "eregion resources schedule set appdb --node main --every-seconds 86400 --keep-last 7 --json"),
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{
				use:  "set <name>",
				meta: resourceMutationMeta("Record a backup policy for the orchestrator's River periodic job", "eregion resources schedule set appdb --node main --cron '0 3 * * *' --timezone UTC --keep-last 7 --verify-after --idempotency-key ops:appdb-schedule --json"),
				args: resourceOneName,
				flags: func(cmd *cobra.Command) {
					addResourceFollowFlags(cmd)
					cmd.Flags().String("node", "", "Explicit application host when the name exists on more than one")
					cmd.Flags().Int64("every-seconds", 0, "Interval in seconds (60–366 days); mutually exclusive with --cron")
					cmd.Flags().String("cron", "", "Five-field cron expression; mutually exclusive with --every-seconds")
					cmd.Flags().String("timezone", "", "IANA timezone for cron (default UTC)")
					cmd.Flags().Int("keep-last", 0, "Keep the newest N archives")
					cmd.Flags().Int64("max-age-seconds", 0, "Keep archives younger than N seconds")
					cmd.Flags().Bool("verify-after", false, "Declare that the orchestrator verifies each backup after creation")
				},
				run: func(cmd *cobra.Command, args []string) (result, error) {
					spec, err := a.resourceSpec(cmd, args[0])
					if err != nil {
						return result{}, err
					}
					every, _ := cmd.Flags().GetInt64("every-seconds")
					cron, _ := cmd.Flags().GetString("cron")
					timezone, _ := cmd.Flags().GetString("timezone")
					keepLast, _ := cmd.Flags().GetInt("keep-last")
					maxAge, _ := cmd.Flags().GetInt64("max-age-seconds")
					verifyAfter, _ := cmd.Flags().GetBool("verify-after")
					policy := resources.Policy{
						Schedule:    resources.Schedule{EverySeconds: every, Cron: cron, Timezone: timezone},
						Retention:   resources.Retention{KeepLast: keepLast, MaxAgeSeconds: maxAge},
						VerifyAfter: verifyAfter,
					}
					return a.submitResource(cmd, resources.KindBackupSchedule, "node/"+spec.Host, resources.BackupScheduleInput{Spec: spec, Policy: policy})
				},
			},
		},
	}
}

func resourceSpecFromFlags(cmd *cobra.Command, name string) (resources.Spec, error) {
	host, _ := cmd.Flags().GetString("host")
	version, _ := cmd.Flags().GetString("version")
	database, _ := cmd.Flags().GetString("database")
	username, _ := cmd.Flags().GetString("username")
	address, _ := cmd.Flags().GetString("address")
	port, _ := cmd.Flags().GetInt("port")
	volume, _ := cmd.Flags().GetString("volume")
	if strings.TrimSpace(host) == "" {
		return resources.Spec{}, contract.NewFault(contract.InputRequired, "--host is required", "Choose the explicit application host; placement is never inferred.")
	}
	if strings.TrimSpace(database) == "" {
		return resources.Spec{}, contract.NewFault(contract.InputRequired, "--database is required", "Name the database created by the PostgreSQL image.")
	}
	if strings.TrimSpace(username) == "" {
		return resources.Spec{}, contract.NewFault(contract.InputRequired, "--username is required", "Name the application database username.")
	}
	spec := resources.Spec{
		Name: name, Engine: resources.EnginePostgreSQL, Version: version, Host: host,
		Address: address, Port: port, Database: database, Username: username,
		Volume: volume, Protection: resources.ProtectionApplication,
	}
	if err := resources.ValidateSpec(spec); err != nil {
		return resources.Spec{}, err
	}
	return spec, nil
}

// resourceItem resolves one resource name to its full stored spec through the
// read-only inventory. A duplicate name on several hosts is ambiguous; callers
// must disambiguate with --node rather than guessing.
func (a *app) resourceItem(cmd *cobra.Command, name string) (json.RawMessage, error) {
	filters := url.Values{"limit": {"200"}}
	if node, _ := cmd.Flags().GetString("node"); node != "" {
		filters.Set("node", node)
	}
	raw, err := a.platformReadInventoryWith(cmd, "resources", filters)
	if err != nil {
		return nil, err
	}
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, contract.NewFault(contract.Unavailable, "API returned an invalid resources page", "Check binary compatibility; do not assume the resource is absent.")
	}
	var matches []json.RawMessage
	for _, item := range page.Items {
		var identity struct {
			Spec resources.Spec `json:"spec"`
		}
		if json.Unmarshal(item, &identity) == nil && identity.Spec.Name == name {
			matches = append(matches, item)
		}
	}
	switch len(matches) {
	case 0:
		return nil, contract.NewFault(contract.NotFound, "resource not found", "List resources to choose an existing name and host.")
	case 1:
		return matches[0], nil
	default:
		return nil, contract.NewFault(contract.Conflict, "resource name exists on more than one host", "Pass --node with the explicit application host.")
	}
}

func (a *app) resourceSpec(cmd *cobra.Command, name string) (resources.Spec, error) {
	item, err := a.resourceItem(cmd, name)
	if err != nil {
		return resources.Spec{}, err
	}
	var identity struct {
		Spec resources.Spec `json:"spec"`
	}
	if err := json.Unmarshal(item, &identity); err != nil || identity.Spec.Name == "" {
		return resources.Spec{}, contract.NewFault(contract.Unavailable, "API returned an unreadable resource identity", "Check binary compatibility before submitting a mutation.")
	}
	return identity.Spec, nil
}

func resourceInventoryFilters(cmd *cobra.Command) url.Values {
	filters := url.Values{}
	for _, name := range []string{"node", "state"} {
		if flag := cmd.Flags().Lookup(name); flag != nil {
			value, _ := cmd.Flags().GetString(name)
			if value != "" {
				filters.Set(name, value)
			}
		}
	}
	if flag := cmd.Flags().Lookup("limit"); flag != nil {
		limit, _ := cmd.Flags().GetInt("limit")
		filters.Set("limit", fmt.Sprintf("%d", limit))
	}
	return filters
}

func (a *app) resourceConnectInfo(cmd *cobra.Command, name string) (result, error) {
	item, err := a.resourceItem(cmd, name)
	if err != nil {
		return result{}, err
	}
	var observation struct {
		Spec       resources.Spec                 `json:"spec"`
		State      resources.State                `json:"state"`
		Endpoint   *resources.Endpoint            `json:"endpoint,omitempty"`
		Credential *resources.CredentialReference `json:"credential,omitempty"`
	}
	if err := json.Unmarshal(item, &observation); err != nil {
		return result{}, contract.NewFault(contract.Unavailable, "API returned an unreadable resource observation", "Check binary compatibility.")
	}
	if observation.Endpoint == nil {
		return result{}, contract.NewFault(contract.PreconditionFailed, "resource has no published endpoint", "Create or recover the resource and inspect resources status before connecting.")
	}
	connection := fmt.Sprintf("postgres://%s@%s:%d/%s?sslmode=%s",
		observation.Endpoint.Username, observation.Endpoint.Address,
		observation.Endpoint.Port, observation.Endpoint.Database, observation.Endpoint.SSLMode)
	data := map[string]any{
		"resource":      observation.Spec,
		"state":         observation.State,
		"endpoint":      observation.Endpoint,
		"credential":    observation.Credential,
		"connection":    connection,
		"secret_source": "reference the protected credential with the vault secret set/export command; never inline the password",
		// The environment receives a pinned vault reference, not this value.
		// Replace <app>, <environment>, and <revision> with the destination.
		"secret_ref_template":   "vault:<app>/<environment>/DATABASE_URL@<revision>",
		"never_inline_password": true,
	}
	encoded, _ := json.Marshal(data)
	plain := fmt.Sprintf("Resource %s on %s (%s)\nconnection: %s\n", observation.Spec.Name, observation.Spec.Host, observation.State, connection)
	if observation.Credential != nil {
		plain += fmt.Sprintf("password_file: %s (mode %s; contents are never printed)\n", observation.Credential.PasswordFile, observation.Credential.Mode)
	}
	plain += "environment: create or update the app environment with a secret reference such as DATABASE_URL, not the plaintext password\n"
	return result{data: json.RawMessage(encoded), plain: plain}, nil
}

func (a *app) submitResource(cmd *cobra.Command, kind, target string, input any) (result, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return result{}, contract.NewFault(contract.Internal, "could not encode resource input", "Report the command and binary version.")
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	follow, _ := cmd.Flags().GetBool("follow")
	if dryRun && follow {
		return result{}, contract.NewFault(contract.Usage, "--follow cannot be combined with --dry-run", "Submit without --dry-run to follow a durable operation.")
	}
	if follow {
		if err := validateFollow(cmd); err != nil {
			return result{}, err
		}
	}
	key, err := resourceIdempotencyKey(cmd, kind, target, raw, dryRun)
	if err != nil {
		return result{}, err
	}
	submitted, err := a.submitPlatformRawMode(cmd, kind, target, raw, key, dryRun)
	if err != nil {
		return submitted, err
	}
	if dryRun {
		submitted.plain = resourcePreviewPlain(submitted.data, "")
	}
	if !follow {
		return submitted, nil
	}
	return a.followSubmittedResource(cmd, submitted)
}

func (a *app) submitResourceDestructive(cmd *cobra.Command, kind, target string, input any) (result, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return result{}, contract.NewFault(contract.Internal, "could not encode resource input", "Report the command and binary version.")
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	follow, _ := cmd.Flags().GetBool("follow")
	if dryRun && follow {
		return result{}, contract.NewFault(contract.Usage, "--follow cannot be combined with --dry-run", "Submit without --dry-run to follow a durable operation.")
	}
	if follow {
		if err := validateFollow(cmd); err != nil {
			return result{}, err
		}
	}
	token := platformConfirmationToken(kind, target, raw)
	if dryRun {
		preview, err := a.submitPlatformRawMode(cmd, kind, target, raw, "", true)
		if err != nil {
			return preview, err
		}
		previewRaw, _ := preview.data.(json.RawMessage)
		preview.data, _ = platformWithConfirmation(previewRaw, token)
		preview.plain = resourcePreviewPlain(preview.data, token)
		return preview, nil
	}
	confirmation, _ := cmd.Flags().GetString("confirm")
	if confirmation == "" {
		return result{}, contract.NewFault(contract.ConfirmationRequired, "--confirm is required for this destructive action", "Run the command with --dry-run and pass the returned confirmation token.")
	}
	if subtle.ConstantTimeCompare([]byte(confirmation), []byte(token)) != 1 {
		return result{}, contract.NewFault(contract.PreconditionFailed, "confirmation token does not match this plan", "Run with --dry-run and pass the confirmation token for the exact resource, host, and input.")
	}
	key, err := resourceIdempotencyKey(cmd, kind, target, raw, false)
	if err != nil {
		return result{}, err
	}
	submitted, err := a.submitPlatformRawMode(cmd, kind, target, raw, key, false)
	if err != nil {
		return submitted, err
	}
	if !follow {
		return submitted, nil
	}
	return a.followSubmittedResource(cmd, submitted)
}

// resourceIdempotencyKey uses the explicit flag when present. Otherwise it
// derives a stable key from kind, target, and canonical input. A repeated
// identical payload with no explicit key is therefore the same operation;
// callers who intentionally repeat work (for example a second backup) pass a
// fresh --idempotency-key.
func resourceIdempotencyKey(cmd *cobra.Command, kind, target string, raw json.RawMessage, dryRun bool) (string, error) {
	if dryRun {
		return "", nil
	}
	if key, _ := cmd.Flags().GetString("idempotency-key"); key != "" {
		if err := contract.ValidateIdempotencyKey(key); err != nil {
			return "", err
		}
		return key, nil
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(kind))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(target))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(raw)
	return "resources:" + kind + ":" + hex.EncodeToString(digest.Sum(nil)[:16]), nil
}

func (a *app) followSubmittedResource(cmd *cobra.Command, submitted result) (result, error) {
	raw, ok := submitted.data.(json.RawMessage)
	if !ok {
		return result{}, contract.NewFault(contract.Unavailable, "API returned unreadable operation data", "Check binary compatibility; do not assume an unrecognized result means work was not accepted.")
	}
	var operation contract.Operation
	if json.Unmarshal(raw, &operation) != nil || operation.Validate() != nil {
		return result{}, contract.NewFault(contract.Unavailable, "API returned unreadable operation data", "Check binary compatibility; do not assume an unrecognized result means work was not accepted.")
	}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	return a.followOperation(cmd, client, operation.ID)
}

// resourcePreviewPlain renders the extension's nested static plan. The generic
// platform presenter only knows top-level step names; resources returns the
// ordered plan under input.steps, including destructive markers.
func resourcePreviewPlain(data any, token string) string {
	raw, ok := data.(json.RawMessage)
	if !ok {
		encoded, err := json.Marshal(data)
		if err != nil {
			return ""
		}
		raw = encoded
	}
	var wrapper struct {
		Preview json.RawMessage `json:"preview"`
	}
	selected := raw
	if json.Unmarshal(raw, &wrapper) == nil && len(wrapper.Preview) > 0 {
		selected = wrapper.Preview
	}
	var preview struct {
		Kind     string   `json:"kind"`
		Target   string   `json:"target"`
		DryRun   bool     `json:"dry_run"`
		Summary  string   `json:"summary"`
		Warnings []string `json:"warnings"`
		Input    struct {
			Resource             string               `json:"resource"`
			Host                 string               `json:"host"`
			Steps                []resources.PlanStep `json:"steps"`
			Destructive          bool                 `json:"destructive"`
			ConfirmationRequired bool                 `json:"confirmation_required"`
			ConfirmationPhrase   string               `json:"confirmation_phrase"`
			SafetyBackup         bool                 `json:"safety_backup"`
		} `json:"input"`
	}
	if err := json.Unmarshal(selected, &preview); err != nil || !preview.DryRun {
		return platformPretty(raw)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Dry run: %s on %s\n", preview.Kind, preview.Target)
	if preview.Input.Resource != "" {
		fmt.Fprintf(&out, "resource: %s on %s\n", preview.Input.Resource, preview.Input.Host)
	}
	if preview.Summary != "" {
		out.WriteString(preview.Summary + "\n")
	}
	for _, step := range preview.Input.Steps {
		marker := "step"
		if step.Destructive {
			marker = "destructive step"
		}
		fmt.Fprintf(&out, "  %s: %s\n", marker, step.Description)
	}
	if preview.Input.SafetyBackup {
		out.WriteString("  safety backup: required before cutover\n")
	}
	if preview.Input.ConfirmationRequired {
		fmt.Fprintf(&out, "confirmation: repeat the exact phrase %q\n", preview.Input.ConfirmationPhrase)
	}
	for _, warning := range preview.Warnings {
		fmt.Fprintf(&out, "  warning: %s\n", warning)
	}
	if token != "" {
		out.WriteString("confirmation_token: " + token + "\n")
	}
	return out.String()
}

func resourceTable(raw json.RawMessage) string {
	var page struct {
		Items []struct {
			Spec     resources.Spec      `json:"spec"`
			State    resources.State     `json:"state"`
			Endpoint *resources.Endpoint `json:"endpoint,omitempty"`
			Health   *resources.Health   `json:"health,omitempty"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil || len(page.Items) == 0 {
		return "(none)\n"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%-20s %-16s %-10s %-22s %-25s\n", "NAME", "HOST", "STATE", "ENDPOINT", "HEALTH")
	for _, item := range page.Items {
		endpoint := "-"
		if item.Endpoint != nil {
			endpoint = fmt.Sprintf("%s:%d", item.Endpoint.Address, item.Endpoint.Port)
		}
		health := "-"
		if item.Health != nil {
			health = string(item.Health.Status)
		}
		fmt.Fprintf(&out, "%-20s %-16s %-10s %-22s %-25s\n", item.Spec.Name, item.Spec.Host, item.State, endpoint, health)
	}
	return out.String()
}

func backupTable(raw json.RawMessage) string {
	var page struct {
		Items []resources.BackupArtifact `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil || len(page.Items) == 0 {
		return "(none)\n"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%-48s %-20s %-10s %-10s %s\n", "ARCHIVE", "RESOURCE", "REASON", "VERIFIED", "CREATED")
	for _, item := range page.Items {
		fmt.Fprintf(&out, "%-48s %-20s %-10s %-10t %s\n", filepath.Base(item.Archive), item.Resource, item.Reason, item.Verified, item.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	return out.String()
}

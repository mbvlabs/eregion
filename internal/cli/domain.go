package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/spf13/cobra"
)

// domainSpecs is `eregion domain`: public hostnames that Caddy on the main
// node routes to an environment, each with its own ACME certificate.
func (a *app) domainSpecs() []spec {
	parent := platformMeta("Route public hostnames to environments through Caddy", "eregion domain list --json")
	add := platformMutationMeta("Route a public hostname to an environment", "eregion domain add hello production hello.example.com")
	add.WhenToUse = []string{"Give an environment a public HTTPS URL. Create the DNS record it shows; Caddy gets the certificate once it resolves."}
	remove := platformMutationMeta("Remove a public hostname's route", "eregion domain remove hello production hello.example.com")
	submission := func(cmd *cobra.Command) {
		cmd.Flags().Bool("dry-run", false, "Validate with the API and return a plan without accepting an operation")
		cmd.Flags().String("idempotency-key", "", "Stable submission key; one is generated when empty")
	}
	return []spec{{
		use:  "domain",
		meta: parent,
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{use: "add <app> <environment> <hostname>", meta: add, args: cobra.ExactArgs(3), flags: submission,
				run: func(cmd *cobra.Command, args []string) (result, error) {
					return a.domainChange(cmd, platform.KindDomainAdd, args)
				}},
			{use: "remove <app> <environment> <hostname>", meta: remove, args: cobra.ExactArgs(3), flags: submission,
				run: func(cmd *cobra.Command, args []string) (result, error) {
					return a.domainChange(cmd, platform.KindDomainRemove, args)
				}},
			{use: "list", meta: platformMeta("List public hostnames and the environments they route to", "eregion domain list --json"), args: platformReadArgs,
				flags: addInventoryFlags,
				run:   a.domainList},
		},
	}}
}

func (a *app) domainChange(cmd *cobra.Command, kind string, args []string) (result, error) {
	target, err := platform.NewEnvironmentTarget(args[0], args[1])
	if err != nil {
		return result{}, err
	}
	input := platform.DomainInput{App: args[0], Environment: args[1], Domain: args[2]}
	normalized, err := platform.Default().Normalize(kind, target.String(), mustJSON(input))
	if err != nil {
		return result{}, err
	}
	domain := normalized.Input.(*platform.DomainInput).Domain
	if dry, _ := cmd.Flags().GetBool("dry-run"); dry {
		return a.submitPlatformRawMode(cmd, kind, target.String(), normalized.Canonical, "", true)
	}
	key, _ := cmd.Flags().GetString("idempotency-key")
	if key == "" {
		key = "cli:" + strings.TrimPrefix(kind, "platform.") + ":" + randomHex(8)
	}
	if err := contract.ValidateIdempotencyKey(key); err != nil {
		return result{}, err
	}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	raw, err := client.SubmitAction(cmd.Context(), kind, target.String(), normalized.Canonical, key, false)
	if err != nil {
		return result{}, err
	}
	var op contract.Operation
	if err := json.Unmarshal(raw, &op); err != nil {
		return result{}, contract.NewFault(contract.Internal, "unexpected API answer", err.Error())
	}
	op, err = a.waitOperation(cmd.Context(), op.ID, time.Minute)
	if err != nil {
		return result{}, err
	}
	if op.State != contract.Succeeded {
		return result{}, contract.NewFault(contract.Unavailable, "the domain change "+string(op.State)+" ("+string(op.ErrorCode)+")", "Inspect it with eregion operations events "+string(op.ID)+".")
	}
	data := map[string]any{"domain": domain, "app": args[0], "environment": args[1], "operation_id": op.ID}
	if kind == platform.KindDomainRemove {
		return result{data: data, plain: "Removed the route for " + domain + "."}, nil
	}
	plain := []string{"Routed https://" + domain + " to " + args[0] + "/" + args[1] + "."}
	if ip := currentPublicIPv4(); ip != "" {
		data["dns_record"] = map[string]string{"type": "A", "name": domain, "value": ip}
		check := checkDNS(cmd.Context(), domain, ip, "")
		data["dns_resolves"] = check.Matches
		plain = append(plain, "", "DNS record:", "  A  "+domain+"  "+ip)
		switch {
		case check.Matches:
			plain = append(plain, "It resolves; Caddy gets the certificate now.")
		case check.Proxied:
			plain = append(plain, "It is proxied by Cloudflare; set it to DNS only so Caddy can get the certificate.")
		default:
			plain = append(plain, "It does not resolve to "+ip+" yet; Caddy gets the certificate once it does.")
		}
	}
	return result{data: data, plain: strings.Join(plain, "\n")}, nil
}

func (a *app) domainList(cmd *cobra.Command, _ []string) (result, error) {
	raw, err := a.platformReadInventory(cmd, "environments")
	if err != nil {
		return result{}, err
	}
	var inv struct {
		Items []platform.Environment `json:"items"`
	}
	if err := json.Unmarshal(raw, &inv); err != nil {
		return result{}, contract.NewFault(contract.Internal, "unexpected inventory answer", err.Error())
	}
	type row struct {
		Domain      string `json:"domain"`
		App         string `json:"app"`
		Environment string `json:"environment"`
		Upstream    string `json:"upstream"`
	}
	rows := []row{}
	var table strings.Builder
	for _, env := range inv.Items {
		for _, d := range env.Domains {
			r := row{d, env.App, env.Name, fmt.Sprintf("127.0.0.1:%d", env.HostPort)}
			rows = append(rows, r)
			fmt.Fprintf(&table, "%-40s %-16s %-16s %s\n", r.Domain, r.App, r.Environment, r.Upstream)
		}
	}
	if len(rows) == 0 {
		return result{data: rows, plain: "No domains. Add one with eregion domain add <app> <environment> <hostname>."}, nil
	}
	return result{data: rows, plain: fmt.Sprintf("%-40s %-16s %-16s %s\n", "DOMAIN", "APP", "ENVIRONMENT", "UPSTREAM") + table.String()}, nil
}

// waitOperation polls an operation until it ends or within passes.
func (a *app) waitOperation(ctx context.Context, id contract.OperationID, within time.Duration) (contract.Operation, error) {
	client, err := a.client(nil)
	if err != nil {
		return contract.Operation{}, err
	}
	deadline := time.Now().Add(within)
	for {
		raw, err := client.Get(ctx, id)
		if err != nil {
			return contract.Operation{}, err
		}
		var op contract.Operation
		if err := json.Unmarshal(raw, &op); err != nil {
			return contract.Operation{}, contract.NewFault(contract.Internal, "unexpected API answer", err.Error())
		}
		if op.State.Terminal() {
			return op, nil
		}
		if time.Now().After(deadline) {
			return op, contract.NewFault(contract.Unavailable, "operation "+string(id)+" is still "+string(op.State), "Follow it with eregion operations events "+string(id)+".")
		}
		select {
		case <-ctx.Done():
			return op, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// currentPublicIPv4 is the main node's public address from the current
// context's WireGuard endpoint.
func currentPublicIPv4() string {
	store, err := host.LoadContextStore("")
	if err != nil || store.Current == "" {
		return ""
	}
	ctx, err := store.Get("")
	if err != nil || ctx.SSH == nil || ctx.SSH.WireGuard == nil {
		return ""
	}
	h, _, err := net.SplitHostPort(ctx.SSH.WireGuard.Endpoint)
	if err != nil {
		return ""
	}
	return h
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

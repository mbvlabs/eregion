package cli

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
)

// certificateWait bounds how long server init waits for Caddy's certificate.
const certificateWait = 90 * time.Second

// publicIPv4 is the address the DNS record points at: the SSH target when it
// is an IPv4 address, otherwise what it resolves to.
func publicIPv4(ctx context.Context, target string) (string, error) {
	if addr, err := netip.ParseAddr(target); err == nil {
		if !addr.Is4() {
			return "", contract.NewFault(contract.ValidationFailed, "server init needs the server's IPv4 address", "Pass --host root@<ipv4>.")
		}
		return addr.String(), nil
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", target)
	if err != nil || len(ips) == 0 {
		return "", contract.NewFault(contract.ValidationFailed, "could not resolve "+target+" to an IPv4 address", "Pass --host root@<ipv4>.")
	}
	return ips[0].Unmap().String(), nil
}

// askDNS shows the A record and, in a terminal, waits for 'confirmed', so the
// record propagates while the host is set up (vaire D10).
func (r *serverInit) askDNS() error {
	if r.dnsShown {
		return nil
	}
	ready := boolFlag(r.cmd, "dns-ready") || dryRun(r.cmd)
	name := r.answers.hostname()
	body := strings.Join(append(hostRecordLines(hostDNSRecords(name, r.publicIPv4, "")),
		"",
		"Create it at your DNS provider; a Cloudflare proxy in front is fine.",
		"eregion checks it after setting up the host; Caddy then gets the certificate."), "\n")
	if r.live == nil {
		if ready {
			r.note("treating the DNS record as created: A " + name + " " + r.publicIPv4)
		} else {
			r.note("create the DNS record: A " + name + " " + r.publicIPv4)
		}
		r.dnsShown = true
		return nil
	}
	r.live.Println(ui.PanelFit("Create this DNS record", body, r.live.Width()))
	r.dnsShown = true
	if ready {
		return nil
	}
	return ui.TypeWord("Type 'confirmed' once the A record exists", "", "confirmed")
}

// checkDNS asks the public resolvers once. A mismatch is a warning: Caddy
// keeps retrying and gets the certificate once the record resolves.
func (r *serverInit) checkDNS(ctx context.Context) {
	name := r.answers.hostname()
	task := r.start("dns", "Checking "+name+" against "+strings.Join(resolverNames(), " and "))
	check := checkDNS(ctx, name, r.publicIPv4, "")
	switch {
	case check.Matches:
		task.end(ui.EndOK, "resolves to "+r.publicIPv4)
	case check.Proxied:
		task.end(ui.EndOK, "proxied by Cloudflare")
	default:
		parts := make([]string, 0, len(check.Resolvers))
		for _, a := range check.Resolvers {
			line, _ := a.describe(r.publicIPv4, "")
			parts = append(parts, line)
		}
		task.end(ui.EndWarn, "not resolving yet: "+strings.Join(parts, "; "))
	}
}

func resolverNames() []string {
	out := make([]string, 0, len(publicResolvers))
	for _, s := range publicResolvers {
		out = append(out, strings.TrimSuffix(s, ":53"))
	}
	return out
}

// waitForCertificate polls https://<hostname>/readyz until Caddy serves a
// trusted certificate. Pending is a warning, never a failure.
func (r *serverInit) waitForCertificate(ctx context.Context) {
	name := r.answers.hostname()
	task := r.start("tls", "Waiting for the certificate for "+name)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}
	deadline := time.Now().Add(certificateWait)
	var last error
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+name+"/readyz", nil)
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			var notAfter string
			if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
				notAfter = ", valid until " + resp.TLS.PeerCertificates[0].NotAfter.Format("2006-01-02")
			}
			task.end(ui.EndOK, "https://"+name+notAfter)
			return
		}
		last = err
		task.detail("Let's Encrypt validates " + name + " over HTTP on port 80")
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	task.end(ui.EndWarn, "still pending; Caddy keeps retrying once DNS points at the server ("+last.Error()+")")
}

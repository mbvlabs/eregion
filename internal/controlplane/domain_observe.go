package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/mbvlabs/eregion/internal/platform"
	"golang.org/x/sync/errgroup"
)

// reconcileDomainRoutes re-renders and applies the Caddy routes from the
// database. A worker with no Caddy configured (development, or before ingress
// setup) skips it; the periodic job retries once ingress exists.
func (s *Service) reconcileDomainRoutes(ctx context.Context) error {
	if _, err := os.Stat(platform.CaddyRoutesPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return s.syncRoutes(ctx)
}

type domainObservation struct {
	name       string
	expected   []string
	healthPath string
}

// observeDomains records DNS, certificate and public-response state without
// changing domain intent. A failed probe never removes the binding.
func (s *Service) observeDomains(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT d.name,d.expected_addresses,
        coalesce(NULLIF(r.runtime->>'health_path',''),e.health_path,'/')
        FROM eregion_platform_domains d
        JOIN eregion_platform_environments e ON e.id=d.environment_id
        LEFT JOIN eregion_platform_environment_revisions r ON r.id=e.current_revision_id
        ORDER BY d.name LIMIT 50`)
	if err != nil {
		return unavailable()
	}
	var domains []domainObservation
	for rows.Next() {
		var domain domainObservation
		if err := rows.Scan(&domain.name, &domain.expected, &domain.healthPath); err != nil {
			rows.Close()
			return unavailable()
		}
		domains = append(domains, domain)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return unavailable()
	}
	if len(domains) == 0 {
		return nil
	}
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(4)
	for _, domain := range domains {
		domain := domain
		group.Go(func() error {
			return s.observeDomain(groupCtx, domain)
		})
	}
	return group.Wait()
}

func (s *Service) observeDomain(ctx context.Context, domain domainObservation) error {
	now := time.Now().UTC()
	dnsState := platform.DNSPending
	dnsAddresses := []string{}
	dnsError := ""
	dnsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	dnsObservation := platform.LookupPublicDNS(dnsCtx, domain.name)
	cancel()
	dnsAddresses = dnsObservation.Addresses
	dnsError = dnsObservation.Error
	if len(domain.expected) == 0 {
		dnsError = "the main node's public address was not recorded; re-run domain add"
	} else {
		dnsState = platform.ClassifyDNS(domain.expected, dnsObservation.Addresses)
	}

	certificateState := platform.CertificatePending
	certificateError := "waiting for DNS to point at the main node"
	var certificateNotAfter *time.Time
	publicState := platform.PublicPending
	publicError := "waiting for DNS to point at the main node"
	publicStatus := 0
	certificateObservedAt := (*time.Time)(nil)
	publicObservedAt := (*time.Time)(nil)
	if dnsState == platform.DNSOK {
		certCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		certificate := platform.ObserveCertificate(certCtx, domain.name, platform.CaddyHTTPS)
		cancel()
		certificateState, certificateError, certificateNotAfter = certificate.State, certificate.Error, certificate.NotAfter
		observed := time.Now().UTC()
		certificateObservedAt = &observed

		publicCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		public := platform.ObservePublic(publicCtx, domain.name, domain.healthPath, platform.CaddyHTTPS)
		cancel()
		publicState, publicError, publicStatus = public.State, public.Error, public.Status
		observed = time.Now().UTC()
		publicObservedAt = &observed
	}
	if dnsError == "" && dnsState == platform.DNSWrong {
		dnsError = "the public resolvers do not return the recorded address"
	}
	if dnsError == "" && dnsState == platform.DNSProxied {
		dnsError = "the record is proxied by Cloudflare; set it to DNS only"
	}
	var statusArg any
	if publicStatus != 0 {
		statusArg = publicStatus
	}
	_, err := s.pool.Exec(ctx, `UPDATE eregion_platform_domains
        SET dns_state=$2,dns_addresses=$3,dns_error=$4,dns_observed_at=$5,
            certificate_state=$6,certificate_not_after=$7,certificate_error=$8,certificate_observed_at=$9,
            public_state=$10,public_status=$11,public_error=$12,public_observed_at=$13,
            observed_at=$14
        WHERE name=$1`, domain.name, dnsState, dnsAddresses, boundedRouteError(dnsError), now,
		certificateState, certificateNotAfter, boundedRouteError(certificateError), certificateObservedAt,
		publicState, statusArg, boundedRouteError(publicError), publicObservedAt, now)
	if err != nil {
		return unavailable()
	}
	return nil
}

func logDomainMaintenance(what string, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	slog.WarnContext(context.Background(), "domain "+what+" failed", "error", err)
}

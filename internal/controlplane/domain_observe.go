package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
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

const domainObservationPageSize = 100

// observeDomains records DNS, certificate and public-response state without
// changing domain intent. A failed probe never removes the binding. The
// observer describes the current desired binding, so its default path is the
// current revision's health path; a deploy's public check uses the deployment's
// pinned revision instead.
func (s *Service) observeDomains(ctx context.Context) error {
	return walkDomainObservationPages(ctx, domainObservationPageSize,
		func(ctx context.Context, after string, limit int) ([]domainObservation, error) {
			rows, err := s.pool.Query(ctx, `SELECT d.name,d.expected_addresses,
                coalesce(NULLIF(d.check_path,''), NULLIF(r.runtime->>'health_path',''), e.health_path, '/')
                FROM eregion_platform_domains d
                JOIN eregion_platform_environments e ON e.id=d.environment_id
                LEFT JOIN eregion_platform_environment_revisions r ON r.id=e.current_revision_id
                WHERE d.name > $1
                ORDER BY d.name LIMIT $2`, after, limit)
			if err != nil {
				return nil, unavailable()
			}
			defer rows.Close()
			var domains []domainObservation
			for rows.Next() {
				var domain domainObservation
				if err := rows.Scan(&domain.name, &domain.expected, &domain.healthPath); err != nil {
					return nil, unavailable()
				}
				domains = append(domains, domain)
			}
			if err := rows.Err(); err != nil {
				return nil, unavailable()
			}
			return domains, nil
		},
		func(groupCtx context.Context, domains []domainObservation) error {
			group, groupCtx := errgroup.WithContext(groupCtx)
			group.SetLimit(4)
			for _, domain := range domains {
				domain := domain
				group.Go(func() error {
					return s.observeDomain(groupCtx, domain)
				})
			}
			return group.Wait()
		})
}

// walkDomainObservationPages walks every domain in name order. Keyset paging
// keeps the read bounded without silently stopping at the first page.
func walkDomainObservationPages(
	ctx context.Context,
	pageSize int,
	fetch func(context.Context, string, int) ([]domainObservation, error),
	apply func(context.Context, []domainObservation) error,
) error {
	if pageSize < 1 {
		return contract.NewFault(contract.Internal, "domain observation page size is invalid", "Fix the worker's domain observation page size.")
	}
	after := ""
	for {
		page, err := fetch(ctx, after, pageSize)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		if err := apply(ctx, page); err != nil {
			return err
		}
		if len(page) < pageSize {
			return nil
		}
		after = page[len(page)-1].name
	}
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
	if platform.DNSReachesNode(dnsState) {
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

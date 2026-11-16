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
			rows, err := s.entities.Platform().Domains().ObservationTargets(ctx, after, limit)
			if err != nil {
				return nil, unavailable(err)
			}
			domains := make([]domainObservation, 0, len(rows))
			for _, row := range rows {
				domains = append(domains, domainObservation{
					name: row.Domain, expected: row.ExpectedAddresses, healthPath: row.CheckPath,
				})
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
	dnsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	dnsObservation := platform.LookupPublicDNS(dnsCtx, domain.name)
	cancel()
	dnsAddresses := dnsObservation.Addresses
	dnsError := dnsObservation.Error
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
	if dnsAddresses == nil {
		dnsAddresses = []string{}
	}
	if err := validateDomainObservationWrite(dnsState, certificateState, publicState, publicStatus); err != nil {
		return err
	}
	_, err := s.entities.Platform().Domains().RecordObservation(ctx, domain.name, platform.DomainBinding{
		DNSState: dnsState, DNSAddresses: dnsAddresses, DNSError: boundedRouteError(dnsError), DNSObservedAt: &now,
		CertificateState: certificateState, CertificateNotAfter: certificateNotAfter,
		CertificateError: boundedRouteError(certificateError), CertificateObservedAt: certificateObservedAt,
		PublicState: publicState, PublicStatus: publicStatus, PublicError: boundedRouteError(publicError),
		PublicObservedAt: publicObservedAt, ObservedAt: &now,
	})
	if err != nil {
		return unavailable(err)
	}
	return nil
}

func validateDomainObservationWrite(dns platform.DNSState, certificate platform.CertificateState, public platform.PublicState, status int) error {
	validDNS := dns == platform.DNSPending || dns == platform.DNSOK || dns == platform.DNSWrong || dns == platform.DNSProxied
	validCert := certificate == platform.CertificatePending || certificate == platform.CertificateValid || certificate == platform.CertificateFailed
	validPublic := public == platform.PublicPending || public == platform.PublicOK || public == platform.PublicFailed
	if !validDNS || !validCert || !validPublic || (status != 0 && (status < 100 || status > 599)) {
		return contract.NewFault(contract.ValidationFailed, "invalid domain observation", "Check the DNS, certificate and public observation before saving.")
	}
	return nil
}

func logDomainMaintenance(ctx context.Context, what string, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	slog.WarnContext(context.WithoutCancel(ctx), "domain "+what+" failed", "error", err)
}

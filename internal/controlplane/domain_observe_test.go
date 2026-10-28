package controlplane

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/platform"
)

func TestWalkDomainObservationPagesCoversEveryDomain(t *testing.T) {
	all := make([]domainObservation, 0, 60)
	for index := 0; index < 60; index++ {
		all = append(all, domainObservation{name: fmt.Sprintf("d%02d.example.com", index), healthPath: "/health"})
	}
	var seen []string
	fetch := func(_ context.Context, after string, limit int) ([]domainObservation, error) {
		page := []domainObservation{}
		for _, domain := range all {
			if domain.name > after {
				page = append(page, domain)
			}
		}
		if len(page) > limit {
			page = page[:limit]
		}
		return page, nil
	}
	apply := func(_ context.Context, page []domainObservation) error {
		for _, domain := range page {
			seen = append(seen, domain.name)
		}
		return nil
	}
	if err := walkDomainObservationPages(context.Background(), 25, fetch, apply); err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(all) {
		t.Fatalf("observed %d of %d domains", len(seen), len(all))
	}
	for index, name := range seen {
		if name != all[index].name {
			t.Fatalf("domain %d = %s, want %s", index, name, all[index].name)
		}
	}
}

func TestWalkDomainObservationPagesStopsOnAnEmptyPage(t *testing.T) {
	calls := 0
	fetch := func(context.Context, string, int) ([]domainObservation, error) {
		calls++
		if calls > 1 {
			t.Fatal("walked past an empty page")
		}
		return nil, nil
	}
	if err := walkDomainObservationPages(context.Background(), 25, fetch, func(context.Context, []domainObservation) error {
		t.Fatal("applied an empty page")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPublicCheckMessageNamesHostPathAndStatus(t *testing.T) {
	if got, want := publicCheckMessage("api.example.com", "/users/sign-in", 200, nil), "public check https://api.example.com/users/sign-in → 200"; got != want {
		t.Fatalf("public check message = %q, want %q", got, want)
	}
	if got, want := publicCheckMessage("api.example.com", "/api/health", 404, nil), "public check https://api.example.com/api/health → 404"; got != want {
		t.Fatalf("public check message = %q, want %q", got, want)
	}
	message := publicCheckMessage("api.example.com", "/api/health", 0, fmt.Errorf("connection refused"))
	if !strings.Contains(message, "→ error: connection refused") {
		t.Fatalf("public check error message = %q", message)
	}
}

func TestPublicCheckPathUsesThePinnedRevision(t *testing.T) {
	env := platform.Environment{
		Revision: &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
			Runtime: platform.RuntimeSpec{HealthPath: "/pinned"},
		}},
		Health: platform.HealthConfig{Path: "/desired"},
	}
	if got := publicCheckPath("", env); got != "/pinned" {
		t.Fatalf("public check path = %q, want the pinned revision path", got)
	}
	if got := publicCheckPath("/alias", env); got != "/alias" {
		t.Fatalf("explicit public check path = %q", got)
	}
	legacy := platform.Environment{Health: platform.HealthConfig{Path: "/health"}}
	if got := publicCheckPath("", legacy); got != "/health" {
		t.Fatalf("legacy public check path = %q", got)
	}
	if got := publicCheckPath("", platform.Environment{}); got != "/" {
		t.Fatalf("default public check path = %q", got)
	}
}

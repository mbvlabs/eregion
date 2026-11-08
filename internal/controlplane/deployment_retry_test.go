package controlplane

import (
	"testing"

	"github.com/mbvlabs/eregion/internal/platform"
)

func TestFrozenPredecessor(t *testing.T) {
	dep := platform.Deployment{ID: "dep_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	previous := platform.DeploymentID("dep_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	serving := platform.DeploymentID("dep_cccccccccccccccccccccccccccccccc")
	for name, tc := range map[string]struct {
		dep            platform.Deployment
		envServing     platform.DeploymentID
		envRelease     platform.ReleaseID
		routeCompleted bool
		wantDeployment platform.DeploymentID
		wantRelease    platform.ReleaseID
	}{
		"unswitched uses live serving": {
			dep: dep, envServing: serving, envRelease: "rel_cccccccccccccccccccccccccccccccc",
			wantDeployment: serving, wantRelease: "rel_cccccccccccccccccccccccccccccccc",
		},
		"completed switch freezes recorded previous": {
			dep:        platform.Deployment{ID: dep.ID, PreviousDeploymentID: previous, PreviousReleaseID: "rel_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			envServing: serving, envRelease: "rel_cccccccccccccccccccccccccccccccc", routeCompleted: true,
			wantDeployment: previous, wantRelease: "rel_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
		"completed switch without previous uses another serving": {
			dep: dep, envServing: serving, envRelease: "rel_cccccccccccccccccccccccccccccccc", routeCompleted: true,
			wantDeployment: serving, wantRelease: "rel_cccccccccccccccccccccccccccccccc",
		},
		"first deploy has no predecessor": {
			dep: dep,
		},
		"serving already names this deployment": {
			dep:            platform.Deployment{ID: dep.ID, PreviousDeploymentID: previous, PreviousReleaseID: "rel_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			envServing:     dep.ID,
			wantDeployment: previous, wantRelease: "rel_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := platform.Environment{ServingDeploymentID: tc.envServing, ServingRelease: tc.envRelease}
			release, deployment := frozenPredecessor(tc.dep, env, tc.routeCompleted)
			if release != tc.wantRelease || deployment != tc.wantDeployment {
				t.Fatalf("frozen predecessor = %s/%s, want %s/%s", release, deployment, tc.wantRelease, tc.wantDeployment)
			}
		})
	}
}

func TestDeploymentLifecycleKind(t *testing.T) {
	for _, kind := range []string{platform.KindDeploy, platform.KindRollback, platform.KindRestart, platform.KindPromote} {
		if !deploymentLifecycleKind(kind) {
			t.Errorf("%s was not treated as a lifecycle operation", kind)
		}
	}
	for _, kind := range []string{platform.KindProcessScale, platform.KindConfigSet, platform.KindDomainAdd} {
		if deploymentLifecycleKind(kind) {
			t.Errorf("%s was treated as a lifecycle operation", kind)
		}
	}
}

package controlplane

import (
	"strings"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

func validWebhookAdmission() api.WebhookAdmission {
	return api.WebhookAdmission{
		DeliveryIDHash: strings.Repeat("a", 64), BodySHA256: strings.Repeat("b", 64),
		Actor: contract.Actor{Kind: "webhook", Subject: "github:mbvlabs/hello"},
		Kind:  platform.KindDeploy, Target: "env/shop/production",
		EnvironmentID: "env_" + strings.Repeat("1", 32), RevisionID: "erv_" + strings.Repeat("2", 32),
		SecretRevision: 7, SecretRevisionHash: strings.Repeat("c", 64),
		Repository: "mbvlabs/hello", Branch: "main", Commit: strings.Repeat("d", 40),
		State: api.WebhookDeliveryAdmitted,
	}
}

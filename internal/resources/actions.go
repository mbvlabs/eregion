package resources

import "encoding/json"

// Platform action kinds. They reuse the generic operation/River path; this
// package never starts a queue or a scheduler.
const (
	KindResourceCreate      = "platform.resource.create"
	KindResourceHealth      = "platform.resource.health"
	KindResourceDestroy     = "platform.resource.destroy"
	KindResourceRecover     = "platform.resource.recover"
	KindResourceCredentials = "platform.resource.credentials"
	KindBackupCreate        = "platform.backup.create"
	KindBackupVerify        = "platform.backup.verify"
	KindBackupRestore       = "platform.backup.restore"
	KindBackupSchedule      = "platform.backup.schedule"
)

// EncodeRequest is the canonical, secret-free payload for a durable action or
// River periodic job. Credentials are never part of Request.
func EncodeRequest(request Request) ([]byte, error) {
	return json.Marshal(request)
}

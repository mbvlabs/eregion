package integrations

import "context"

// GrantStore persists device grants. It is not a queue: operations remain in
// eregion_operations and River. The store only owns access-domain state.
type GrantStore interface {
	List(context.Context) ([]DeviceGrant, error)
	Get(context.Context, string) (DeviceGrant, bool, error)
	Put(context.Context, DeviceGrant) error
}

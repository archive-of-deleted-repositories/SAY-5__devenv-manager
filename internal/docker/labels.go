// Package docker wraps the upstream Docker SDK with the conventions
// devenv-manager relies on for durable identity (labels) and reaping.
package docker

// Label keys applied to every container the service creates. These form
// the durable identity layer that survives service restarts; reaping is
// driven entirely by reading them back from the daemon.
const (
	LabelID           = "com.devenv.id"
	LabelOwner        = "com.devenv.owner"
	LabelCreatedAt    = "com.devenv.created_at"
	LabelLeaseUntil   = "com.devenv.lease_until"
	LabelCreatedByPID = "com.devenv.created_by_pid"
)

// AllLabels returns the full set of label keys the service writes.
// Used for filter construction and validation.
func AllLabels() []string {
	return []string{LabelID, LabelOwner, LabelCreatedAt, LabelLeaseUntil, LabelCreatedByPID}
}

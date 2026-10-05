package objectstore

import "context"

type VersionCursor struct{ KeyMarker, VersionMarker string }
type InventoryVersion struct {
	Ref          VersionRef
	AttemptID    string
	DeleteMarker bool
}
type VersionPage struct {
	Versions  []InventoryVersion
	Next      VersionCursor
	Exhausted bool
}
type VersionPresence string

const (
	VersionPresent VersionPresence = "present"
	VersionAbsent  VersionPresence = "absent"
	VersionUnknown VersionPresence = "unknown"
)

// This capability is separate from Store; upload and scanner callers cannot delete.
type VersionDeleter interface {
	ListVersions(context.Context, Location, VersionCursor, int) (VersionPage, error)
	ProbeVersion(context.Context, VersionRef) (VersionPresence, error)
	DeleteVersion(context.Context, VersionRef) error
}

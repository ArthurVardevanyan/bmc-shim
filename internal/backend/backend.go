package backend

import "context"

type Backend interface {
	PowerOn(ctx context.Context) error
	PowerOff(ctx context.Context) error
}

// PowerStateProvider is an optional interface that backends can implement
// to report the current power state. If not implemented, the server
// will rely on last known in-memory state.
type PowerStateProvider interface {
	CurrentState(ctx context.Context) (on bool, err error)
}

// NameProvider is an optional interface that backends can implement
// to supply a friendly display name for the system.
type NameProvider interface {
	DisplayName(ctx context.Context) (string, error)
}

// HealthChecker is an optional interface that backends can implement
// to report their health status.
type HealthChecker interface {
	Ping(ctx context.Context) error
}

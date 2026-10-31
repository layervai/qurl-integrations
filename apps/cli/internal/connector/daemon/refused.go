package daemon

import "context"

// RunRefusedJob exposes ownership while a native job waits to be replaced.
// It opens no routes and rejects every control mutation.
func RunRefusedJob(ctx context.Context, stateDir, socketPath string) error {
	return (&IPCServer{
		StateDir: stateDir, SocketPath: socketPath, JobVersion: "0/refused",
		Manager: refusedJobManager{}, refused: true,
	}).Run(ctx)
}

type refusedJobManager struct{}

// Run holds ownership until the native manager stops this refused job.
func (refusedJobManager) Run(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

// Trigger cannot reconcile resources in a refused job.
func (refusedJobManager) Trigger() {}

// SetOverlay cannot install headers in a refused job.
func (refusedJobManager) SetOverlay(map[string]map[string]string) {}

// Running reports that a refused job owns no resources.
func (refusedJobManager) Running() map[string]string { return map[string]string{} }

// Diagnostics reports that a refused job opens no routes.
func (refusedJobManager) Diagnostics() map[string]ResourceDiagnostic {
	return map[string]ResourceDiagnostic{}
}

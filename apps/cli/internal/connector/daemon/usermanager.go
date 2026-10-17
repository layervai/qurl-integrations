package daemon

import (
	"errors"
	"fmt"

	connectorservice "github.com/layervai/qurl-connector/pkg/service"
)

// ErrUserServiceManagerUnavailable reports a Linux session with no running
// systemd user manager, which is where native supervision installs the
// background share daemon. Containers, sandboxes, and minimal images commonly
// lack one. The remedy is a different supervision mode (--foreground, or
// --supervision external under another supervisor), so exitcode maps it to
// Config like ErrRuntimeSupervision.
var ErrUserServiceManagerUnavailable = errors.New("no systemd user manager is reachable from this session")

// diagnosedJobManager attributes native job-manager failures to a missing
// systemd user manager when the host evidently has none. Without it, the
// connector's executable search reports every candidate path it rejected,
// which reads like a security failure rather than a missing capability.
//
// TODO(upstream-contract): the interface is embedded, so a method added to
// connectorservice.UserJobManager passes through undiagnosed rather than
// failing to compile. Wrap any new method here.
type diagnosedJobManager struct {
	connectorservice.UserJobManager
	available func() bool
}

func withUserServiceManagerDiagnosis(manager connectorservice.UserJobManager, available func() bool) connectorservice.UserJobManager {
	return diagnosedJobManager{UserJobManager: manager, available: available}
}

// Ensure installs or converges the job, attributing a failure to a missing
// user manager when there is none.
func (m diagnosedJobManager) Ensure(job connectorservice.UserJob) error { //nolint:gocritic // interface requires a value.
	return m.diagnose(m.UserJobManager.Ensure(job))
}

// Replace reinstalls the job, with the same failure attribution.
func (m diagnosedJobManager) Replace(job connectorservice.UserJob) error { //nolint:gocritic // interface requires a value.
	return m.diagnose(m.UserJobManager.Replace(job))
}

// Remove uninstalls the job, with the same failure attribution.
func (m diagnosedJobManager) Remove(label string) error {
	return m.diagnose(m.UserJobManager.Remove(label))
}

// Status reports the job, with the same failure attribution.
func (m diagnosedJobManager) Status(label string) (connectorservice.ServiceStatus, error) {
	status, err := m.UserJobManager.Status(label)
	return status, m.diagnose(err)
}

// diagnose probes only after a failure: a working manager never pays for the
// probe, and a probe that is wrong about a working host cannot turn success
// into an error.
func (m diagnosedJobManager) diagnose(err error) error {
	if err == nil || m.available == nil || m.available() {
		return err
	}
	return fmt.Errorf("%w: %w", ErrUserServiceManagerUnavailable, err)
}

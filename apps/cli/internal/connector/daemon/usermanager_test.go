package daemon

import (
	"errors"
	"testing"

	connectorservice "github.com/layervai/qurl-connector/pkg/service"
)

// failingJobManager fails every call with err, the way the connector's Linux
// manager does when it cannot find a usable systemctl.
type failingJobManager struct{ err error }

func (m failingJobManager) Ensure(connectorservice.UserJob) error  { return m.err }
func (m failingJobManager) Replace(connectorservice.UserJob) error { return m.err }
func (m failingJobManager) Remove(string) error                    { return m.err }
func (m failingJobManager) Status(string) (connectorservice.ServiceStatus, error) {
	return connectorservice.ServiceStatus{Installed: true}, m.err
}

func TestUserServiceManagerDiagnosis(t *testing.T) {
	raw := errors.New("resolve trusted systemctl control plane: validate systemctl candidate /bin/systemctl: directory component /bin must not be a symlink")
	calls := func(manager connectorservice.UserJobManager) map[string]error {
		_, statusErr := manager.Status(DaemonJobLabel)
		return map[string]error{
			"Ensure":  manager.Ensure(connectorservice.UserJob{}),
			"Replace": manager.Replace(connectorservice.UserJob{}),
			"Remove":  manager.Remove(DaemonJobLabel),
			"Status":  statusErr,
		}
	}

	t.Run("no user manager attributes every failure", func(t *testing.T) {
		manager := withUserServiceManagerDiagnosis(failingJobManager{err: raw}, func() bool { return false })
		for method, err := range calls(manager) {
			if !errors.Is(err, ErrUserServiceManagerUnavailable) || !errors.Is(err, raw) {
				t.Errorf("%s error = %v, want ErrUserServiceManagerUnavailable wrapping the connector error", method, err)
			}
		}
		if status, _ := manager.Status(DaemonJobLabel); !status.Installed {
			t.Error("Status must still return the manager's status alongside the diagnosis")
		}
	})

	t.Run("running user manager keeps the connector error", func(t *testing.T) {
		manager := withUserServiceManagerDiagnosis(failingJobManager{err: raw}, func() bool { return true })
		for method, err := range calls(manager) {
			if err != raw { //nolint:errorlint // identity is the contract: unchanged means the same value.
				t.Errorf("%s error = %v, want the connector error unchanged", method, err)
			}
		}
	})

	t.Run("success never probes", func(t *testing.T) {
		probed := false
		manager := withUserServiceManagerDiagnosis(failingJobManager{}, func() bool { probed = true; return false })
		for method, err := range calls(manager) {
			if err != nil {
				t.Errorf("%s error = %v, want nil", method, err)
			}
		}
		if probed {
			t.Error("a successful call must not probe for the user manager")
		}
	})
}

func TestNewJobControllerDiagnosesTheNativeManager(t *testing.T) {
	controller := newTestJobController(t, t.TempDir(), t.TempDir(), "3.0.0", "https://api.example.test", GroupModeSingle, testHubResolver)
	if _, ok := controller.Manager.(diagnosedJobManager); !ok {
		t.Fatalf("production manager = %T, want the user manager diagnosis wrapper", controller.Manager)
	}
}

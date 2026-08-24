package sandbox

import (
	"errors"
	"strings"

	"github.com/envplane/contracts/domain"
)

var ErrUnsafeSandbox = errors.New("sandbox launch policy rejected")

type RuntimePolicy struct {
	RunAsNonRoot             bool
	ReadOnlyRootFilesystem   bool
	AllowPrivilegeEscalation bool
	SeccompProfile           string
	MaxPIDs                  int
	MaxTimeoutSeconds        int
}

func ValidateLaunch(spec domain.AISandboxSpec, policy RuntimePolicy) error {
	if err := spec.Validate(); err != nil {
		return ErrUnsafeSandbox
	}
	if !policy.RunAsNonRoot || !policy.ReadOnlyRootFilesystem || policy.AllowPrivilegeEscalation || !strings.EqualFold(policy.SeccompProfile, "RuntimeDefault") || policy.MaxPIDs <= 0 || spec.Resources.PIDs > policy.MaxPIDs || policy.MaxTimeoutSeconds <= 0 || spec.Resources.TimeoutSeconds > policy.MaxTimeoutSeconds {
		return ErrUnsafeSandbox
	}
	return nil
}

package bootstrap

import (
	"fmt"
	"strings"

	"github.com/envpilot/contracts/domain"
)

type ManagedResourceRuntime struct {
	config        CleanupSafetyConfig
	projectID     string
	environmentID string
	resources     map[string]domain.ResourceSnapshot
}

func NewManagedResourceRuntime(initial []domain.ResourceSnapshot, config CleanupSafetyConfig, projectID string, environmentID string) *ManagedResourceRuntime {
	runtime := &ManagedResourceRuntime{
		config:        config,
		projectID:     strings.TrimSpace(projectID),
		environmentID: strings.TrimSpace(environmentID),
		resources:     map[string]domain.ResourceSnapshot{},
	}
	for _, resource := range initial {
		runtime.resources[resourceRuntimeKey(resource.Kind, resource.Namespace, resource.Name)] = resource
	}
	return runtime
}

func (r *ManagedResourceRuntime) Apply(resource domain.ResourceSnapshot) error {
	if err := ValidateModifyManagedResource(resource, r.projectID, r.environmentID); err != nil {
		return err
	}
	key := resourceRuntimeKey(resource.Kind, resource.Namespace, resource.Name)
	if existing, ok := r.resources[key]; ok {
		if err := ValidateModifyManagedResource(existing, r.projectID, r.environmentID); err != nil {
			return err
		}
	}
	r.resources[key] = resource
	return nil
}

func (r *ManagedResourceRuntime) Delete(kind string, namespace string, name string) error {
	key := resourceRuntimeKey(kind, namespace, name)
	existing, ok := r.resources[key]
	if !ok {
		return nil
	}
	if err := ValidateDeleteManagedResource(existing, r.config, r.projectID, r.environmentID); err != nil {
		return err
	}
	delete(r.resources, key)
	return nil
}

func (r *ManagedResourceRuntime) Get(kind string, namespace string, name string) (domain.ResourceSnapshot, bool) {
	resource, ok := r.resources[resourceRuntimeKey(kind, namespace, name)]
	return resource, ok
}

func resourceRuntimeKey(kind string, namespace string, name string) string {
	return fmt.Sprintf("%s/%s/%s", strings.TrimSpace(kind), strings.TrimSpace(namespace), strings.TrimSpace(name))
}

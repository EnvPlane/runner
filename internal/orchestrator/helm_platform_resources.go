package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"strconv"
	"strings"

	"github.com/envplane/contracts/domain"
	"gopkg.in/yaml.v3"
)

var helmPlatformAPIVersions = map[string]string{
	"NetworkPolicy": "networking.k8s.io/v1", "Ingress": "networking.k8s.io/v1",
	"ResourceQuota": "v1", "LimitRange": "v1",
}

const helmPlatformPlanOwner = "envplane.io/platform-resource-owner"

type helmPlatformExecutor interface {
	TemplateServices(context.Context, HelmUpgradeOptions) ([]map[string]any, error)
	PutPlatformResource(context.Context, domain.RenderedResource, string) (map[string]any, error)
	GetPlatformResource(context.Context, string, string, string) (map[string]any, error)
}

func renderHelmPlatformResources(environment domain.Environment, projectConfig domain.ProjectConfig, namespace string) ([]Manifest, error) {
	templates, err := helmPlatformTemplates(projectConfig)
	if err != nil || len(templates) == 0 {
		return nil, err
	}
	if namespace != "envplane-pr-"+environment.ID {
		return nil, errors.New("compiled Helm platform resources require the dedicated environment namespace")
	}
	var result []Manifest
	seen := make(map[string]bool)
	data := compiledManifestTemplateData(environment)
	for _, item := range templates {
		apiVersion := helmPlatformAPIVersions[item.Kind]
		raw, err := executeCompiledManifestTemplate(item.YAML, data)
		if err != nil {
			return nil, fmt.Errorf("render compiled Helm %s: %w", item.Kind, err)
		}
		object, err := strictPlatformManifest(raw)
		if err != nil || platformString(object, "kind") != item.Kind || platformString(object, "apiVersion") != apiVersion {
			return nil, fmt.Errorf("invalid compiled Helm %s manifest", item.Kind)
		}
		meta := platformMap(object, "metadata")
		name := platformString(meta, "name")
		sourceNamespace := platformString(meta, "namespace")
		canonicalTemplateNamespace := "envplane-pr-" + strings.TrimSpace(environment.Source.PullRequestID)
		if !validPlatformName(name) || (sourceNamespace != "" && sourceNamespace != namespace &&
			(strings.TrimSpace(environment.Source.PullRequestID) == "" || sourceNamespace != canonicalTemplateNamespace ||
				strings.TrimSpace(item.Namespace) != "envplane-pr-{{ .PRNumber }}")) {
			return nil, fmt.Errorf("compiled Helm %s has invalid identity or namespace", item.Kind)
		}
		if seen[item.Kind+"/"+name] {
			return nil, fmt.Errorf("duplicate compiled Helm %s named %q", item.Kind, name)
		}
		seen[item.Kind+"/"+name] = true
		meta["namespace"] = namespace
		if _, err := platformObject(object, item.Kind, namespace, name); err != nil {
			return nil, err
		}
		payload, err := json.Marshal(object)
		if err != nil {
			return nil, err
		}
		result = append(result, Manifest{Path: item.Path, Kind: item.Kind, Content: payload})
	}
	return result, nil
}

func helmPlatformTemplates(projectConfig domain.ProjectConfig) ([]compiledManifestTemplate, error) {
	raw := platformMap(projectConfig.Config, "bootstrapSessionData")["manifestTemplates"]
	if raw == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, errors.New("cannot encode compiled Helm platform templates")
	}
	var templates []compiledManifestTemplate
	if err := json.Unmarshal(encoded, &templates); err != nil {
		return nil, errors.New("cannot decode compiled Helm platform templates")
	}
	reviewed := templates[:0]
	for _, item := range templates {
		if helmPlatformAPIVersions[item.Kind] != "" {
			reviewed = append(reviewed, item)
		}
	}
	return reviewed, nil
}

func strictPlatformManifest(raw string) (map[string]any, error) {
	decoder := yaml.NewDecoder(strings.NewReader(raw))
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("expected exactly one compiled manifest object")
	}
	return object, nil
}

func platformMap(value map[string]any, key string) map[string]any {
	object, _ := value[key].(map[string]any)
	return object
}

func platformString(value map[string]any, key string) string {
	str, _ := value[key].(string)
	return str
}

func validPlatformName(name string) bool {
	return len(name) <= 253 && secretMetadataNamePattern.MatchString(name)
}

// Only desired state is accepted. In particular, ownerReferences, finalizers,
// resourceVersion, and Helm adoption annotations cannot come from a template.
func platformObject(object map[string]any, kind, namespace, name string) (map[string]any, error) {
	meta := platformMap(object, "metadata")
	if !validPlatformName(name) || helmPlatformAPIVersions[kind] == "" ||
		platformString(object, "apiVersion") != helmPlatformAPIVersions[kind] || platformString(object, "kind") != kind ||
		platformString(meta, "name") != name || platformString(meta, "namespace") != namespace || platformMap(object, "spec") == nil {
		return nil, fmt.Errorf("invalid Helm platform resource identity: %s/%s", kind, name)
	}
	for key := range object {
		switch key {
		case "apiVersion", "kind", "metadata", "spec":
		default:
			return nil, fmt.Errorf("unsupported Helm platform resource field %q", key)
		}
	}
	for key := range meta {
		switch key {
		case "name", "namespace", "labels", "annotations":
		default:
			return nil, fmt.Errorf("unsupported Helm platform metadata field %q", key)
		}
	}
	for key := range platformMap(meta, "annotations") {
		if key == helmPlatformPlanOwner || strings.HasPrefix(key, "meta.helm.sh/") || strings.HasPrefix(key, "helm.sh/") {
			return nil, errors.New("compiled Helm platform resource contains reserved ownership annotations")
		}
	}
	return object, nil
}

func (b *HelmDirectBackend) signedPlatformResources(env domain.Environment, config domain.ProjectConfig, plan *domain.EnvironmentReleasePlan) ([]domain.RenderedResource, error) {
	expected, err := helmPlatformTemplates(config)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		if len(expected) != 0 {
			return nil, errors.New("compiled Helm platform resources require a signed release plan")
		}
		return nil, nil
	}
	var resources []domain.RenderedResource
	want := make(map[string]int)
	for _, item := range expected {
		want[item.Kind]++
	}
	for _, item := range plan.RenderedResources {
		if helmPlatformAPIVersions[item.Kind] == "" {
			continue
		}
		if item.Namespace != "envplane-pr-"+env.ID || item.Namespace != env.Namespace {
			return nil, errors.New("signed Helm platform resource is outside the dedicated preview namespace")
		}
		if _, err := platformObject(item.Manifest, item.Kind, item.Namespace, item.Name); err != nil {
			return nil, err
		}
		want[item.Kind]--
		resources = append(resources, item)
	}
	for _, count := range want {
		if count != 0 {
			return nil, errors.New("signed Helm plan is missing or differs from compiled platform resources; create a fresh environment")
		}
	}
	return resources, nil
}

// ApplySignedPlatformRelease consumes only the already verified signed plan.
// Install policy and admission limits before workloads, and publish ingress
// only after verifying its backend belongs to this exact Helm release.
func (b *HelmDirectBackend) ApplySignedPlatformRelease(ctx context.Context, env domain.Environment, config domain.ProjectConfig, plan *domain.EnvironmentReleasePlan) error {
	resources, err := b.signedPlatformResources(env, config, plan)
	if err != nil {
		return err
	}
	if len(resources) == 0 {
		return b.Apply(ctx, env, config)
	}
	executor, ok := b.helmExecutor.(helmPlatformExecutor)
	if !ok {
		return errors.New("cannot apply signed Helm platform resources with this executor")
	}
	releaseName, namespace, err := b.DeploymentTarget(env, config)
	if err != nil {
		return err
	}
	owned, err := b.helmExecutor.IsNamespaceManaged(ctx, namespace, env.Project, env.ID)
	if err != nil || !owned {
		return errors.New("owned preview namespace is required for Helm platform resources")
	}
	observed := make(map[string]map[string]any, len(resources))
	for _, item := range resources {
		if item.Kind == "Ingress" {
			continue
		}
		spec, err := executor.PutPlatformResource(ctx, item, plan.PlanID)
		if err != nil {
			return err
		}
		observed[item.ResourceID] = spec
	}
	if err := b.Apply(ctx, env, config); err != nil {
		return err
	}
	for _, item := range resources {
		if item.Kind != "Ingress" {
			continue
		}
		if err := verifyPlatformIngressServices(ctx, executor, item, releaseName); err != nil {
			return err
		}
		spec, err := executor.PutPlatformResource(ctx, item, plan.PlanID)
		if err != nil {
			return err
		}
		observed[item.ResourceID] = spec
	}
	for _, item := range resources {
		live, err := executor.GetPlatformResource(ctx, item.Kind, item.Namespace, item.Name)
		if err != nil || !ownedPlatformResource(live, item, plan.PlanID) ||
			!reflect.DeepEqual(platformMap(live, "spec"), observed[item.ResourceID]) {
			return fmt.Errorf("readback failed for Helm platform %s/%s", item.Kind, item.Name)
		}
	}
	return nil
}

// VerifySignedPlatformRelease prevents a later Helm status poll from changing
// a release with missing managed resources back to Ready.
func (b *HelmDirectBackend) VerifySignedPlatformRelease(ctx context.Context, env domain.Environment, config domain.ProjectConfig, plan *domain.EnvironmentReleasePlan) error {
	resources, err := b.signedPlatformResources(env, config, plan)
	if err != nil || len(resources) == 0 {
		return err
	}
	executor, ok := b.helmExecutor.(helmPlatformExecutor)
	if !ok {
		return errors.New("cannot verify signed Helm platform resources with this executor")
	}
	releaseName, namespace, err := b.DeploymentTarget(env, config)
	if err != nil {
		return err
	}
	owned, err := b.helmExecutor.IsNamespaceManaged(ctx, namespace, env.Project, env.ID)
	if err != nil || !owned {
		return errors.New("owned preview namespace is required for Helm platform resources")
	}
	for _, item := range resources {
		live, err := executor.GetPlatformResource(ctx, item.Kind, item.Namespace, item.Name)
		if err != nil || !ownedPlatformResource(live, item, plan.PlanID) {
			return fmt.Errorf("readback failed for Helm platform %s/%s", item.Kind, item.Name)
		}
		if item.Kind == "Ingress" {
			if err := verifyPlatformIngressServices(ctx, executor, item, releaseName); err != nil {
				return err
			}
		}
	}
	return nil
}

// HelmPlatformReadbackReceipt is the non-secret identity/digest receipt for
// the exact signed resources verified by the Runner. The manifests themselves
// are not sent back with an apply or status result.
func HelmPlatformReadbackReceipt(plan *domain.EnvironmentReleasePlan) []domain.RenderedResource {
	if plan == nil {
		return nil
	}
	var receipt []domain.RenderedResource
	for _, item := range plan.RenderedResources {
		if helmPlatformAPIVersions[item.Kind] != "" {
			item.Manifest = nil
			receipt = append(receipt, item)
		}
	}
	return receipt
}

func ownedPlatformResource(live map[string]any, item domain.RenderedResource, owner string) bool {
	meta := platformMap(live, "metadata")
	return platformString(live, "apiVersion") == helmPlatformAPIVersions[item.Kind] &&
		platformString(live, "kind") == item.Kind && platformString(meta, "name") == item.Name &&
		platformString(meta, "namespace") == item.Namespace && meta["deletionTimestamp"] == nil &&
		platformString(platformMap(meta, "annotations"), helmPlatformPlanOwner) == owner
}

func (e *CLIHelmExecutor) GetPlatformResource(ctx context.Context, kind, namespace, name string) (map[string]any, error) {
	resource := strings.ToLower(kind)
	switch kind {
	case "Service", "ResourceQuota", "LimitRange":
	case "Ingress", "NetworkPolicy":
		resource += ".networking.k8s.io"
	default:
		return nil, errors.New("unsupported Helm platform readback kind")
	}
	if !validPlatformName(namespace) || !validPlatformName(name) {
		return nil, errors.New("invalid Helm platform readback identity")
	}
	output, err := e.namespaceRead(ctx, "get", resource, name, "--namespace", namespace, "--ignore-not-found", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("read Helm platform %s/%s", kind, name)
	}
	if len(bytes.TrimSpace(output)) == 0 {
		return nil, nil
	}
	var object map[string]any
	if err := json.Unmarshal(output, &object); err != nil {
		return nil, fmt.Errorf("decode Helm platform %s/%s", kind, name)
	}
	return object, nil
}

func (e *CLIHelmExecutor) PutPlatformResource(ctx context.Context, item domain.RenderedResource, owner string) (map[string]any, error) {
	if _, err := platformObject(item.Manifest, item.Kind, item.Namespace, item.Name); err != nil {
		return nil, err
	}
	live, err := e.GetPlatformResource(ctx, item.Kind, item.Namespace, item.Name)
	if err != nil {
		return nil, err
	}
	meta := make(map[string]any)
	for key, value := range platformMap(item.Manifest, "metadata") {
		meta[key] = value
	}
	annotations := make(map[string]any)
	for key, value := range platformMap(meta, "annotations") {
		annotations[key] = value
	}
	annotations[helmPlatformPlanOwner] = owner
	meta["annotations"] = annotations
	operation := "create"
	if live != nil {
		if !ownedPlatformResource(live, item, owner) {
			return nil, fmt.Errorf("refusing to adopt existing %s/%s", item.Kind, item.Name)
		}
		version := platformString(platformMap(live, "metadata"), "resourceVersion")
		if version == "" {
			return nil, errors.New("missing Helm platform resource version")
		}
		meta["resourceVersion"] = version
		operation = "replace"
	}
	body, err := json.Marshal(map[string]any{"apiVersion": helmPlatformAPIVersions[item.Kind], "kind": item.Kind, "metadata": meta, "spec": item.Manifest["spec"]})
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "kubectl", operation, "--namespace", item.Namespace, "-f", "-", "-o", "json")
	cmd.Stdin = bytes.NewReader(body)
	data, err := cmd.Output()
	var stored map[string]any
	if err != nil || json.Unmarshal(data, &stored) != nil || !ownedPlatformResource(stored, item, owner) || platformMap(stored, "spec") == nil {
		return nil, fmt.Errorf("write Helm platform %s/%s was not verified", item.Kind, item.Name)
	}
	return platformMap(stored, "spec"), nil
}

func (e *CLIHelmExecutor) TemplateServices(ctx context.Context, options HelmUpgradeOptions) ([]map[string]any, error) {
	if !domain.IsSafeHelmChartRef(options.ChartRef) {
		return nil, errors.New("invalid Helm chart reference for Ingress binding")
	}
	args := []string{"template", options.ReleaseName, options.ChartRef, "--namespace", options.Namespace}
	if options.ValuesFile != "" {
		args = append(args, "-f", options.ValuesFile)
	}
	if options.ChartVersion != "" {
		args = append(args, "--version", options.ChartVersion)
	}
	rendered, err := e.runCommand(ctx, "helm", args...)
	if err != nil {
		return nil, errors.New("render Helm chart Services for compiled Ingress")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(rendered))
	var services []map[string]any
	for {
		var object map[string]any
		if err := decoder.Decode(&object); errors.Is(err, io.EOF) {
			return services, nil
		} else if err != nil {
			return nil, errors.New("decode Helm chart Service inventory")
		}
		if platformString(object, "kind") == "Service" && platformString(object, "apiVersion") == "v1" {
			services = append(services, object)
		}
	}
}

func bindPlatformIngress(manifests []Manifest, services []map[string]any, namespace string) error {
	backends := make(map[string][]map[string]any)
	objects := make(map[int]map[string]any)
	for i, item := range manifests {
		if item.Kind != "Ingress" {
			continue
		}
		object, err := strictPlatformManifest(string(item.Content))
		if err != nil {
			return err
		}
		objects[i] = object
		refs, err := platformIngressBackends(object)
		if err != nil {
			return err
		}
		for _, ref := range refs {
			name := platformString(ref, "name")
			backends[name] = append(backends[name], ref)
		}
	}
	for logicalName, refs := range backends {
		var exact, candidates []string
		var declared int
		var exactDeclared bool
		for _, svc := range services {
			meta := platformMap(svc, "metadata")
			name, ns := platformString(meta, "name"), platformString(meta, "namespace")
			if !validPlatformName(name) || (ns != "" && ns != namespace) {
				continue
			}
			declared++
			exactDeclared = exactDeclared || name == logicalName
			matches := true
			for _, ref := range refs {
				matches = matches && platformServicePortMatches(svc, platformMap(ref, "port"))
			}
			if matches {
				candidates = append(candidates, name)
				if name == logicalName {
					exact = append(exact, name)
				}
			}
		}
		var resolved string
		switch {
		case len(exact) == 1:
			resolved = exact[0]
		case !exactDeclared && declared == 1 && len(backends) == 1 && len(candidates) == 1:
			resolved = candidates[0]
		default:
			return fmt.Errorf("compiled Ingress backend %q does not map uniquely to the Helm chart", logicalName)
		}
		for _, ref := range refs {
			ref["name"] = resolved
		}
	}
	for i, object := range objects {
		body, err := json.Marshal(object)
		if err != nil {
			return err
		}
		manifests[i].Content = body
	}
	return nil
}

func platformIngressBackends(object map[string]any) ([]map[string]any, error) {
	var refs []map[string]any
	spec := platformMap(object, "spec")
	appendBackend := func(backend map[string]any) error {
		svc := platformMap(backend, "service")
		if !validPlatformName(platformString(svc, "name")) || platformMap(svc, "port") == nil || backend["resource"] != nil {
			return errors.New("compiled Helm Ingress must reference a named Service and port")
		}
		refs = append(refs, svc)
		return nil
	}
	if spec["defaultBackend"] != nil {
		if err := appendBackend(platformMap(spec, "defaultBackend")); err != nil {
			return nil, err
		}
	}
	rules, _ := spec["rules"].([]any)
	for _, rule := range rules {
		entry, _ := rule.(map[string]any)
		paths, _ := platformMap(entry, "http")["paths"].([]any)
		for _, path := range paths {
			entry, _ := path.(map[string]any)
			if err := appendBackend(platformMap(entry, "backend")); err != nil {
				return nil, err
			}
		}
	}
	if len(refs) == 0 {
		return nil, errors.New("compiled Helm Ingress has no Service backend")
	}
	return refs, nil
}

func platformServicePortMatches(service, requested map[string]any) bool {
	spec := platformMap(service, "spec")
	if platformString(spec, "type") == "ExternalName" || len(platformMap(spec, "selector")) == 0 {
		return false
	}
	ports, _ := spec["ports"].([]any)
	for _, value := range ports {
		port, _ := value.(map[string]any)
		if name := platformString(requested, "name"); name != "" {
			if name == platformString(port, "name") {
				return true
			}
		} else if number, err := strconv.Atoi(fmt.Sprint(requested["number"])); err == nil && number > 0 && fmt.Sprint(port["port"]) == strconv.Itoa(number) {
			return true
		}
	}
	return false
}

func verifyPlatformIngressServices(ctx context.Context, executor helmPlatformExecutor, item domain.RenderedResource, releaseName string) error {
	refs, err := platformIngressBackends(item.Manifest)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		name := platformString(ref, "name")
		svc, err := executor.GetPlatformResource(ctx, "Service", item.Namespace, name)
		meta := platformMap(svc, "metadata")
		owner := platformMap(meta, "annotations")
		if err != nil || platformString(svc, "kind") != "Service" || platformString(meta, "name") != name ||
			platformString(meta, "namespace") != item.Namespace || platformString(owner, "meta.helm.sh/release-name") != releaseName ||
			platformString(owner, "meta.helm.sh/release-namespace") != item.Namespace ||
			!platformServicePortMatches(svc, platformMap(ref, "port")) {
			return fmt.Errorf("compiled Ingress backend %q is not verified in Helm release %q", name, releaseName)
		}
	}
	return nil
}

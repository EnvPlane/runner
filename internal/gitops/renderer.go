package gitops

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/envpilot/contracts/domain"
)

type FluxOptions struct {
	FluxNamespace   string
	SourceRefName   string
	DependsOnName   string
	ProductBasePath string
	HealthCheckName string
	AppChartVersion string
	InfraVersion    string
	NginxVersion    string
}

type Renderer interface {
	Render(environment domain.Environment) ([]byte, error)
	RenderNamespace(environment domain.Environment) ([]byte, error)
	RenderPathKustomization(environment domain.Environment) ([]byte, error)
	RenderManifestSet(environment domain.Environment) ([]Manifest, error)
}

type Manifest struct {
	Path    string
	Kind    string
	Content []byte
}

type FluxRenderer struct {
	options FluxOptions
}

func NewFluxRenderer(options FluxOptions) FluxRenderer {
	return FluxRenderer{options: options}
}

type substitute struct {
	Key   string
	Value string
}

type renderData struct {
	Environment    domain.Environment
	Options        FluxOptions
	AppPath        string
	Substitute     []substitute
	DeployServices []domain.ServiceOverride
}

func (d renderData) SourceRefName() string {
	if d.Environment.GitOps.SourceRefName != "" {
		return d.Environment.GitOps.SourceRefName
	}
	return d.Options.SourceRefName
}

func (d renderData) HealthCheckName() string {
	if d.Environment.GitOps.HealthCheckName != "" {
		return d.Environment.GitOps.HealthCheckName
	}
	return d.Options.HealthCheckName
}

func (d renderData) HealthCheckEnabled() bool {
	healthCheckName := strings.TrimSpace(strings.ToLower(d.Environment.GitOps.HealthCheckName))
	if healthCheckName == "" {
		return true
	}
	switch healthCheckName {
	case "off", "false", "disable", "disabled", "0", "no":
		return false
	default:
		return true
	}
}

func (d renderData) TargetNamespace() string {
	return strings.TrimSpace(d.Environment.GitOps.TargetNamespace)
}

func (d renderData) TargetNamespaceEnabled() bool {
	return d.TargetNamespace() != ""
}

func (d renderData) ChartName() string {
	name := strings.TrimSpace(d.Environment.GitOps.HealthCheckName)
	if name == "" {
		name = strings.TrimSpace(d.Environment.Product)
	}
	if name == "" {
		name = strings.TrimSpace(d.Environment.ID)
	}
	return name
}

func (d renderData) ValuesFiles() []string {
	if strings.TrimSpace(d.Environment.GitOps.ValuesPath) == "" {
		return nil
	}
	return []string{strings.TrimSpace(d.Environment.GitOps.ValuesPath)}
}

func (d renderData) OverlayResourcePath() string {
	path := strings.TrimSpace(d.AppPath)
	if path == "" {
		return path
	}
	if strings.HasPrefix(path, ".") ||
		strings.HasPrefix(path, "/") ||
		strings.Contains(path, "://") ||
		strings.HasPrefix(path, "git::") {
		return path
	}
	return "../../../" + strings.TrimPrefix(path, "/")
}

func (r FluxRenderer) Render(environment domain.Environment) ([]byte, error) {
	if environment.ID == "" {
		return nil, fmt.Errorf("environment id is required")
	}
	if environment.Product == "" {
		return nil, fmt.Errorf("product is required")
	}

	data := renderData{
		Environment: environment,
		Options:     r.options,
		AppPath:     environment.GitOps.Path,
		Substitute:  buildSubstitutions(environment, r.options),
	}
	if data.AppPath == "" {
		data.AppPath = strings.TrimSuffix(r.options.ProductBasePath, "/") + "/" + environment.Product
	}

	var output bytes.Buffer
	if err := fluxTemplate.Execute(&output, data); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (r FluxRenderer) RenderNamespace(environment domain.Environment) ([]byte, error) {
	if environment.ID == "" {
		return nil, fmt.Errorf("environment id is required")
	}
	data := namespaceData{
		Name:        namespaceManifestName(environment),
		Environment: environment,
	}

	var output bytes.Buffer
	if err := namespaceTemplate.Execute(&output, data); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (r FluxRenderer) RenderPathKustomization(environment domain.Environment) ([]byte, error) {
	if environment.ID == "" {
		return nil, fmt.Errorf("environment id is required")
	}
	manifest := "flux-kustomization.yaml"
	switch rendererKind(environment.GitOps.Renderer) {
	case "helm":
		manifest = "helm-release.yaml"
	case "raw":
		manifest = "raw-manifests.yaml"
	case "kustomize-overlay":
		manifest = "overlay"
	}
	data := pathKustomizationData{
		Resources: []string{
			"namespace.yaml",
			manifest,
		},
	}

	var output bytes.Buffer
	if err := pathKustomizationTemplate.Execute(&output, data); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (r FluxRenderer) RenderManifestSet(environment domain.Environment) ([]Manifest, error) {
	namespace, err := r.RenderNamespace(environment)
	if err != nil {
		return nil, err
	}
	appManifest, kind, err := r.renderAppManifest(environment)
	if err != nil {
		return nil, err
	}
	pathKustomization, err := r.RenderPathKustomization(environment)
	if err != nil {
		return nil, err
	}
	return []Manifest{
		{Path: environment.NamespaceManifestFilename(), Kind: "Namespace", Content: namespace},
		{Path: environment.ManifestFilename(), Kind: kind, Content: appManifest},
		{Path: environment.PathKustomizationFilename(), Kind: "Kustomization", Content: pathKustomization},
	}, nil
}

func (r FluxRenderer) renderAppManifest(environment domain.Environment) ([]byte, string, error) {
	switch rendererKind(environment.GitOps.Renderer) {
	case "helm":
		content, err := r.RenderHelmRelease(environment)
		return content, "HelmRelease", err
	case "raw":
		content, err := r.RenderRawManifests(environment)
		return content, "RawManifests", err
	case "kustomize-overlay":
		content, err := r.RenderKustomizeOverlay(environment)
		return content, "KustomizeOverlay", err
	default:
		content, err := r.Render(environment)
		return content, "FluxKustomization", err
	}
}

func (r FluxRenderer) RenderHelmRelease(environment domain.Environment) ([]byte, error) {
	if environment.ID == "" {
		return nil, fmt.Errorf("environment id is required")
	}
	if environment.Product == "" {
		return nil, fmt.Errorf("product is required")
	}
	data := renderData{
		Environment: environment,
		Options:     r.options,
		AppPath:     environment.GitOps.Path,
		Substitute:  buildSubstitutions(environment, r.options),
	}
	if data.AppPath == "" {
		data.AppPath = strings.TrimSuffix(r.options.ProductBasePath, "/") + "/" + environment.Product
	}

	var output bytes.Buffer
	if err := helmReleaseTemplate.Execute(&output, data); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func rendererKind(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "helm", "helm-release":
		return "helm"
	case "raw", "raw-manifests", "raw-manifest":
		return "raw"
	case "kustomize", "kustomize-overlay", "overlay":
		return "kustomize-overlay"
	default:
		return "flux-kustomization"
	}
}

func (r FluxRenderer) RenderRawManifests(environment domain.Environment) ([]byte, error) {
	if environment.ID == "" {
		return nil, fmt.Errorf("environment id is required")
	}
	data := renderData{
		Environment:    environment,
		Options:        r.options,
		Substitute:     buildSubstitutions(environment, r.options),
		DeployServices: deploymentServices(environment),
	}
	var output bytes.Buffer
	if err := rawManifestsTemplate.Execute(&output, data); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (r FluxRenderer) RenderKustomizeOverlay(environment domain.Environment) ([]byte, error) {
	if environment.ID == "" {
		return nil, fmt.Errorf("environment id is required")
	}
	data := renderData{
		Environment: environment,
		Options:     r.options,
		AppPath:     environment.GitOps.Path,
		Substitute:  buildSubstitutions(environment, r.options),
	}
	if data.AppPath == "" {
		data.AppPath = strings.TrimSuffix(r.options.ProductBasePath, "/") + "/" + environment.Product
	}
	var output bytes.Buffer
	if err := kustomizeOverlayTemplate.Execute(&output, data); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (r FluxRenderer) RenderValuesPreview(environment domain.Environment) map[string]string {
	items := buildSubstitutions(environment, r.options)
	values := make(map[string]string, len(items))
	for _, item := range items {
		values[item.Key] = item.Value
	}
	return values
}

func ValuesYAML(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var output strings.Builder
	for _, key := range keys {
		output.WriteString(key)
		output.WriteString(": ")
		output.WriteString(values[key])
		output.WriteString("\n")
	}
	return output.String()
}

func deploymentServices(environment domain.Environment) []domain.ServiceOverride {
	services := make([]domain.ServiceOverride, 0, len(environment.Services))
	for _, service := range environment.Services {
		if strings.TrimSpace(service.Name) == "" {
			continue
		}
		if environment.Mode == domain.ModeHybrid && !service.Replace {
			continue
		}
		services = append(services, service)
	}
	return services
}

func NamespaceName(id string) string {
	return "envpilot-pr-" + strings.Trim(strings.ToLower(id), "-")
}

func namespaceManifestName(environment domain.Environment) string {
	if strings.TrimSpace(environment.Namespace) != "" {
		return strings.TrimSpace(environment.Namespace)
	}
	if strings.TrimSpace(environment.Source.PullRequestID) != "" {
		return NamespaceName(environment.Source.PullRequestID)
	}
	return NamespaceName(environment.ID)
}

func buildSubstitutions(environment domain.Environment, options FluxOptions) []substitute {
	infra := environment.Infrastructure
	charts := environment.Charts
	if charts.App == "" {
		charts.App = options.AppChartVersion
	}
	if charts.Infra == "" {
		charts.Infra = options.InfraVersion
	}
	if charts.Nginx == "" {
		charts.Nginx = options.NginxVersion
	}
	if infra.Zone == "" {
		infra.Zone = "ca-central-1b"
	}
	if infra.Capacity == "" {
		infra.Capacity = "spot"
	}

	domainName := environment.Domain
	if domainName == "" {
		domainName = environment.ID + ".feature.int"
	}
	base := environment.Base
	if strings.TrimSpace(base.EnvironmentID) == "" {
		base.EnvironmentID = "feature"
	}
	if strings.TrimSpace(base.Namespace) == "" {
		base.Namespace = base.EnvironmentID
	}
	servicePlan := buildServicePlan(environment, base.Namespace)

	values := map[string]string{
		"appChartVersion":    charts.App,
		"infraChartVersion":  charts.Infra,
		"nginxChartVersion":  charts.Nginx,
		"root_env":           base.EnvironmentID,
		"baseNamespace":      base.Namespace,
		"baseDomain":         base.Domain,
		"baseServices":       baseServicesValue(base.Namespace, base.Services),
		"deployServices":     strings.Join(servicePlan.Deploy, ","),
		"baseRoutedServices": baseRoutedServicesValue(servicePlan.Base),
		"routingStrategy":    routingStrategy(environment.Mode),
		"overrideRoutes":     overrideRoutesValue(servicePlan.Deploy, environment.Namespace),
		"fallbackRoutes":     baseRoutedServicesValue(servicePlan.Base),
		"replacedServices":   strings.Join(servicePlan.Deploy, ","),
		"env":                environment.ID,
		"mode":               string(environment.Mode),
		"mysqlEnabled":       quoteBool(infra.MySQL),
		"postgresEnabled":    quoteBool(infra.Postgres),
		"rabbitmqEnabled":    quoteBool(infra.RabbitMQ),
		"redisEnabled":       quoteBool(infra.Redis),
		"memcachedEnabled":   quoteBool(infra.Memcached),
		"mongodbEnabled":     quoteBool(infra.MongoDB),
		"externalDomain":     "'false'",
		"internalDomain":     "'true'",
		"ingressEnabled":     "'true'",
		"ingressHost":        domainName,
		"capacityType":       infra.Capacity,
		"private_domain":     domainName,
		"main_domain":        domainName,
		"previewHost":        domainName,
		"previewUrl":         "https://" + domainName,
		"ZONE":               infra.Zone,
		"servicesSecret":     "backend-services-passwd",
		"imageTag":           "latest",
		"nginxTag":           "latest",
		"phpTag":             "latest",
		"nuxtTag":            "latest",
		"bettingTag":         "latest",
		"websocketsTag":      "latest",
		"cmsApiTag":          "latest",
		"cmsBackendTag":      "latest",
		"cmsFrontendTag":     "latest",
		"cmsMigrationTag":    "latest",
		"cmsRestTag":         "latest",
		"apiNotificationTag": "latest",
		"apiHeimdallTag":     "latest",
		"apiCursusTag":       "latest",
		"apiIrisTag":         "latest",
	}

	for _, service := range environment.Services {
		key := serviceTagKey(service.Name)
		if key == "" || service.Tag == "" {
			continue
		}
		values[key] = service.Tag
	}
	for _, service := range servicePlan.All {
		values[serviceDeployEnabledKey(service)] = quoteBool(servicePlan.DeploySet[service])
		if servicePlan.DeploySet[service] {
			values[serviceRouteNamespaceKey(service)] = environment.Namespace
			values[serviceRouteTargetKey(service)] = "override"
		}
		if namespace := servicePlan.BaseNamespace[service]; namespace != "" {
			values[serviceBaseNamespaceKey(service)] = namespace
			values[serviceRouteNamespaceKey(service)] = namespace
			values[serviceRouteTargetKey(service)] = "base"
		}
	}
	for key, value := range environment.Overrides {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		values[key] = strings.TrimSpace(value)
	}

	items := make([]substitute, 0, len(values))
	for key, value := range values {
		items = append(items, substitute{Key: key, Value: value})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Key < items[j].Key
	})
	return items
}

type servicePlan struct {
	All           []string
	Deploy        []string
	Base          []serviceBaseRoute
	DeploySet     map[string]bool
	BaseNamespace map[string]string
}

type serviceBaseRoute struct {
	Name      string
	Namespace string
}

func buildServicePlan(environment domain.Environment, baseNamespace string) servicePlan {
	baseNamespace = strings.TrimSpace(baseNamespace)
	if baseNamespace == "" {
		baseNamespace = "feature"
	}

	plan := servicePlan{
		DeploySet:     map[string]bool{},
		BaseNamespace: map[string]string{},
	}
	known := map[string]struct{}{}
	for _, service := range environment.Services {
		name := normalizeServiceName(service.Name)
		if name == "" {
			continue
		}
		if _, ok := known[name]; !ok {
			known[name] = struct{}{}
			plan.All = append(plan.All, name)
		}
		deploy := environment.Mode == domain.ModeFull || service.Replace
		if deploy {
			plan.DeploySet[name] = true
			continue
		}
		plan.BaseNamespace[name] = baseNamespace
	}
	for _, service := range environment.Base.Services {
		name := normalizeServiceName(service.Name)
		if name == "" {
			continue
		}
		if _, ok := known[name]; !ok {
			known[name] = struct{}{}
			plan.All = append(plan.All, name)
		}
		if plan.DeploySet[name] {
			continue
		}
		namespace := strings.TrimSpace(service.Namespace)
		if namespace == "" {
			namespace = baseNamespace
		}
		plan.BaseNamespace[name] = namespace
	}
	sort.Strings(plan.All)
	for _, name := range plan.All {
		if plan.DeploySet[name] {
			plan.Deploy = append(plan.Deploy, name)
			continue
		}
		if namespace := plan.BaseNamespace[name]; namespace != "" {
			plan.Base = append(plan.Base, serviceBaseRoute{Name: name, Namespace: namespace})
		}
	}
	sort.Strings(plan.Deploy)
	sort.Slice(plan.Base, func(i, j int) bool {
		return plan.Base[i].Name < plan.Base[j].Name
	})
	return plan
}

func routingStrategy(mode domain.EnvironmentMode) string {
	if mode == domain.ModeFull {
		return "full-ingress"
	}
	return "hybrid-ingress"
}

func overrideRoutesValue(services []string, namespace string) string {
	namespace = strings.TrimSpace(namespace)
	if len(services) == 0 || namespace == "" {
		return ""
	}
	items := make([]string, 0, len(services))
	for _, service := range services {
		service = normalizeServiceName(service)
		if service == "" {
			continue
		}
		items = append(items, service+"="+namespace)
	}
	return strings.Join(items, ",")
}

func baseRoutedServicesValue(routes []serviceBaseRoute) string {
	if len(routes) == 0 {
		return ""
	}
	items := make([]string, 0, len(routes))
	for _, route := range routes {
		if route.Name == "" || route.Namespace == "" {
			continue
		}
		items = append(items, route.Name+"="+route.Namespace)
	}
	return strings.Join(items, ",")
}

func baseServicesValue(defaultNamespace string, services []domain.BaseServiceRef) string {
	if len(services) == 0 {
		return ""
	}
	defaultNamespace = strings.TrimSpace(defaultNamespace)
	items := make([]string, 0, len(services))
	for _, service := range services {
		name := strings.TrimSpace(service.Name)
		if name == "" {
			continue
		}
		namespace := strings.TrimSpace(service.Namespace)
		if namespace == "" {
			namespace = defaultNamespace
		}
		if namespace != "" {
			items = append(items, name+"="+namespace)
			continue
		}
		items = append(items, name)
	}
	return strings.Join(items, ",")
}

func serviceDeployEnabledKey(name string) string {
	return serviceKeyPrefix(name) + "DeployEnabled"
}

func serviceBaseNamespaceKey(name string) string {
	return serviceKeyPrefix(name) + "BaseNamespace"
}

func serviceRouteNamespaceKey(name string) string {
	return serviceKeyPrefix(name) + "RouteNamespace"
}

func serviceRouteTargetKey(name string) string {
	return serviceKeyPrefix(name) + "RouteTarget"
}

func serviceKeyPrefix(name string) string {
	parts := strings.FieldsFunc(normalizeServiceName(name), func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	})
	if len(parts) == 0 {
		return "service"
	}
	var output strings.Builder
	output.WriteString(parts[0])
	for _, part := range parts[1:] {
		if part == "" {
			continue
		}
		output.WriteString(strings.ToUpper(part[:1]))
		if len(part) > 1 {
			output.WriteString(part[1:])
		}
	}
	return output.String()
}

func normalizeServiceName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func quoteBool(value bool) string {
	if value {
		return "'true'"
	}
	return "'false'"
}

func serviceTagKey(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "nginx":
		return "nginxTag"
	case "php", "php-fpm":
		return "phpTag"
	case "nuxt":
		return "nuxtTag"
	case "betting":
		return "bettingTag"
	case "websockets":
		return "websocketsTag"
	case "api", "cms-api":
		return "cmsApiTag"
	case "backend", "cms-backend":
		return "cmsBackendTag"
	case "frontend", "cms-frontend":
		return "cmsFrontendTag"
	case "migration", "cms-migration":
		return "cmsMigrationTag"
	case "rest", "cms-rest":
		return "cmsRestTag"
	case "notifications", "api-notifications":
		return "apiNotificationTag"
	case "heimdall":
		return "apiHeimdallTag"
	case "cursus":
		return "apiCursusTag"
	case "iris":
		return "apiIrisTag"
	default:
		return serviceKeyPrefix(name) + "Tag"
	}
}

var fluxTemplate = template.Must(template.New("flux").Parse(`---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: {{ .Environment.ID }}.{{ .Environment.Product }}
  namespace: {{ .Options.FluxNamespace }}
  labels:
    app.kubernetes.io/managed-by: envpilot
    envpilot.io/project: {{ .Environment.Project }}
    envpilot.io/product: {{ .Environment.Product }}
    envpilot.io/mode: {{ .Environment.Mode }}
spec:
  interval: 30m
  retryInterval: 2m
  timeout: 15m
  wait: true
  prune: true
  dependsOn:
    - name: {{ .Options.DependsOnName }}
  serviceAccountName: kustomize-controller
  sourceRef:
    kind: GitRepository
    name: {{ .SourceRefName }}
  path: {{ .AppPath }}
{{- if .TargetNamespaceEnabled }}
  targetNamespace: {{ .TargetNamespace }}
{{- end }}
{{- if .HealthCheckEnabled }}
  healthChecks:
    - apiVersion: helm.toolkit.fluxcd.io/v2
      kind: HelmRelease
      name: {{ .HealthCheckName }}
      namespace: {{ .Environment.Namespace }}
{{- end }}
  postBuild:
    substituteFrom:
      - kind: ConfigMap
        name: terraform-to-flux-bridge
        optional: true
    substitute:
{{- range .Substitute }}
      {{ .Key }}: {{ .Value }}
{{- end }}
`))

var helmReleaseTemplate = template.Must(template.New("helm-release").Parse(`---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: {{ .Environment.ID }}.{{ .Environment.Product }}
  namespace: {{ .Environment.Namespace }}
  labels:
    app.kubernetes.io/managed-by: envpilot
    envpilot.io/project: {{ .Environment.Project }}
    envpilot.io/product: {{ .Environment.Product }}
    envpilot.io/mode: {{ .Environment.Mode }}
spec:
  interval: 30m
  timeout: 15m
  releaseName: {{ .Environment.ID }}-{{ .Environment.Product }}
  targetNamespace: {{ .Environment.Namespace }}
  chart:
    spec:
      chart: {{ .AppPath }}
      sourceRef:
        kind: GitRepository
        name: {{ .SourceRefName }}
        namespace: {{ .Options.FluxNamespace }}
{{- with .ValuesFiles }}
  valuesFiles:
{{- range . }}
    - {{ . }}
{{- end }}
{{- end }}
  values:
{{- range .Substitute }}
    {{ .Key }}: {{ .Value }}
{{- end }}
`))

var rawManifestsTemplate = template.Must(template.New("raw-manifests").Funcs(template.FuncMap{
	"firstServiceName": firstServiceName,
	"serviceImageTag":  serviceImageTag,
	"quote":            strconvQuote,
}).Parse(`---
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Environment.ID }}-values
  namespace: {{ .Environment.Namespace }}
  labels:
    app.kubernetes.io/managed-by: envpilot
    envpilot.io/project: {{ .Environment.Project }}
data:
{{- range .Substitute }}
  {{ .Key }}: {{ .Value | quote }}
{{- end }}
{{- range .DeployServices }}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Name }}
  namespace: {{ $.Environment.Namespace }}
  labels:
    app.kubernetes.io/name: {{ .Name }}
    app.kubernetes.io/managed-by: envpilot
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: {{ .Name }}
  template:
    metadata:
      labels:
        app.kubernetes.io/name: {{ .Name }}
    spec:
      tolerations:
        - operator: Exists
          effect: NoSchedule
        - operator: Exists
          effect: NoExecute
      containers:
        - name: {{ .Name }}
          image: {{ .Name }}:{{ .Tag | serviceImageTag }}
          ports:
            - containerPort: 8080
---
apiVersion: v1
kind: Service
metadata:
  name: {{ .Name }}
  namespace: {{ $.Environment.Namespace }}
spec:
  selector:
    app.kubernetes.io/name: {{ .Name }}
  ports:
    - port: 80
      targetPort: 8080
{{- end }}
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: {{ .Environment.ID }}
  namespace: {{ .Environment.Namespace }}
spec:
  rules:
    - host: {{ .Environment.Domain }}
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: {{ .DeployServices | firstServiceName }}
                port:
                  number: 80
`))

var kustomizeOverlayTemplate = template.Must(template.New("kustomize-overlay").Funcs(template.FuncMap{
	"quote":            strconvQuote,
	"firstServiceName": firstServiceName,
}).Parse(`---
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: {{ .Environment.Namespace }}
resources:
  - {{ .OverlayResourcePath }}
configMapGenerator:
  - name: envpilot-values
    literals:
{{- range .Substitute }}
      - {{ .Key }}={{ .Value }}
{{- end }}
commonLabels:
  app.kubernetes.io/managed-by: envpilot
  envpilot.io/environment-id: {{ .Environment.ID }}
patches:
  - target:
      kind: Ingress
    patch: |-
      - op: replace
        path: /spec/rules/0/host
        value: {{ .Environment.Domain | quote }}
images:
{{- range .Environment.Services }}
  - name: {{ .Name }}
    newTag: {{ .Tag }}
{{- end }}
`))

func firstServiceName(services []domain.ServiceOverride) string {
	for _, service := range services {
		name := strings.TrimSpace(service.Name)
		if name != "" {
			return name
		}
	}
	return "app"
}

func serviceImageTag(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "latest"
	}
	return value
}

func strconvQuote(value string) string {
	return strconv.Quote(value)
}

type namespaceData struct {
	Name        string
	Environment domain.Environment
}

var namespaceTemplate = template.Must(template.New("namespace").Parse(`---
apiVersion: v1
kind: Namespace
metadata:
  name: {{ .Name }}
  labels:
    app.kubernetes.io/managed-by: envpilot
    envpilot.io/environment-id: {{ .Environment.ID }}
    envpilot.io/project: {{ .Environment.Project }}
    envpilot.io/product: {{ .Environment.Product }}
---
apiVersion: v1
kind: ResourceQuota
metadata:
  name: envpilot-preview-quota
  namespace: {{ .Name }}
  labels:
    app.kubernetes.io/managed-by: envpilot
    envpilot.io/environment-id: {{ .Environment.ID }}
spec:
  hard:
    requests.cpu: "2"
    requests.memory: 4Gi
    limits.cpu: "4"
    limits.memory: 8Gi
    pods: "20"
---
apiVersion: v1
kind: LimitRange
metadata:
  name: envpilot-preview-limits
  namespace: {{ .Name }}
  labels:
    app.kubernetes.io/managed-by: envpilot
    envpilot.io/environment-id: {{ .Environment.ID }}
spec:
  limits:
    - type: Container
      default:
        cpu: 500m
        memory: 512Mi
      defaultRequest:
        cpu: 100m
        memory: 128Mi
`))

type pathKustomizationData struct {
	Resources []string
}

var pathKustomizationTemplate = template.Must(template.New("path-kustomization").Parse(`---
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
{{- range .Resources }}
  - {{ . }}
{{- end }}
`))

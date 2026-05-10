package agent

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
)

type NamespaceSource interface {
	ListNamespaces(ctx context.Context) ([]Namespace, error)
	WatchNamespaces(ctx context.Context, handle func(NamespaceEvent) error) error
}

type WorkloadSource interface {
	ListDeployments(ctx context.Context, namespace string) ([]Deployment, error)
	ListPods(ctx context.Context, namespace string) ([]Pod, error)
	ListIngresses(ctx context.Context, namespace string) ([]Ingress, error)
}

type EventSource interface {
	ListEvents(ctx context.Context, namespace string) ([]KubernetesEvent, error)
}

type FluxSource interface {
	ListFluxKustomizations(ctx context.Context, namespace string) ([]FluxKustomization, error)
	ListHelmReleases(ctx context.Context, namespace string) ([]HelmRelease, error)
	FluxNamespace() string
}

type KubernetesNamespaceSource struct {
	apiURL   string
	token    string
	selector string
	allowed  map[string]struct{}
	fluxNS   string
	client   *http.Client
}

type Namespace struct {
	Metadata NamespaceMetadata `json:"metadata"`
	Status   NamespaceStatus   `json:"status"`
}

type NamespaceMetadata struct {
	Name              string            `json:"name"`
	Labels            map[string]string `json:"labels"`
	DeletionTimestamp string            `json:"deletionTimestamp"`
}

type NamespaceStatus struct {
	Phase string `json:"phase"`
}

type NamespaceEvent struct {
	Type      string
	Namespace Namespace
}

type Deployment struct {
	Metadata DeploymentMetadata `json:"metadata"`
	Spec     DeploymentSpec     `json:"spec"`
	Status   DeploymentStatus   `json:"status"`
}

type DeploymentMetadata struct {
	Name string `json:"name"`
}

type DeploymentSpec struct {
	Replicas *int32 `json:"replicas"`
}

type DeploymentStatus struct {
	Replicas            int32                 `json:"replicas"`
	ReadyReplicas       int32                 `json:"readyReplicas"`
	AvailableReplicas   int32                 `json:"availableReplicas"`
	UpdatedReplicas     int32                 `json:"updatedReplicas"`
	UnavailableReplicas int32                 `json:"unavailableReplicas"`
	Conditions          []DeploymentCondition `json:"conditions"`
}

type DeploymentCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type Pod struct {
	Metadata PodMetadata `json:"metadata"`
	Status   PodStatus   `json:"status"`
}

type PodMetadata struct {
	Name string `json:"name"`
}

type PodStatus struct {
	Phase             string            `json:"phase"`
	Reason            string            `json:"reason"`
	Message           string            `json:"message"`
	Conditions        []PodCondition    `json:"conditions"`
	ContainerStatuses []ContainerStatus `json:"containerStatuses"`
}

type PodCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type ContainerStatus struct {
	Name         string         `json:"name"`
	Ready        bool           `json:"ready"`
	RestartCount int32          `json:"restartCount"`
	State        ContainerState `json:"state"`
}

type ContainerState struct {
	Waiting    *ContainerStateWaiting    `json:"waiting"`
	Terminated *ContainerStateTerminated `json:"terminated"`
}

type ContainerStateWaiting struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type ContainerStateTerminated struct {
	Reason   string `json:"reason"`
	Message  string `json:"message"`
	ExitCode int32  `json:"exitCode"`
}

type Ingress struct {
	Metadata IngressMetadata `json:"metadata"`
	Spec     IngressSpec     `json:"spec"`
	Status   IngressStatus   `json:"status"`
}

type IngressMetadata struct {
	Name string `json:"name"`
}

type IngressSpec struct {
	Rules []IngressRule `json:"rules"`
}

type IngressRule struct {
	Host string `json:"host"`
}

type IngressStatus struct {
	LoadBalancer IngressLoadBalancerStatus `json:"loadBalancer"`
}

type IngressLoadBalancerStatus struct {
	Ingress []LoadBalancerIngress `json:"ingress"`
}

type LoadBalancerIngress struct {
	Hostname string `json:"hostname"`
	IP       string `json:"ip"`
}

type KubernetesEvent struct {
	Metadata       EventMetadata  `json:"metadata"`
	Type           string         `json:"type"`
	Reason         string         `json:"reason"`
	Message        string         `json:"message"`
	InvolvedObject InvolvedObject `json:"involvedObject"`
	Count          int32          `json:"count"`
	FirstTimestamp string         `json:"firstTimestamp"`
	LastTimestamp  string         `json:"lastTimestamp"`
	EventTime      string         `json:"eventTime"`
}

type EventMetadata struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type InvolvedObject struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type FluxKustomization struct {
	Metadata FluxMetadata `json:"metadata"`
	Status   FluxStatus   `json:"status"`
}

type HelmRelease struct {
	Metadata FluxMetadata `json:"metadata"`
	Status   FluxStatus   `json:"status"`
}

type FluxMetadata struct {
	Name       string            `json:"name"`
	Namespace  string            `json:"namespace"`
	Labels     map[string]string `json:"labels"`
	Generation int64             `json:"generation"`
}

type FluxStatus struct {
	ObservedGeneration     int64           `json:"observedGeneration"`
	Conditions             []FluxCondition `json:"conditions"`
	LastAppliedRevision    string          `json:"lastAppliedRevision"`
	LastAttemptedRevision  string          `json:"lastAttemptedRevision"`
	LastHandledReconcileAt string          `json:"lastHandledReconcileAt"`
}

type FluxCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	LastTransitionTime string `json:"lastTransitionTime"`
}

type namespaceList struct {
	Items []Namespace `json:"items"`
}

type namespaceWatchEvent struct {
	Type   string    `json:"type"`
	Object Namespace `json:"object"`
}

type deploymentList struct {
	Items []Deployment `json:"items"`
}

type podList struct {
	Items []Pod `json:"items"`
}

type ingressList struct {
	Items []Ingress `json:"items"`
}

type eventList struct {
	Items []KubernetesEvent `json:"items"`
}

type fluxKustomizationList struct {
	Items []FluxKustomization `json:"items"`
}

type helmReleaseList struct {
	Items []HelmRelease `json:"items"`
}

func NewKubernetesNamespaceSourceFromConfig(cfg Config) (*KubernetesNamespaceSource, error) {
	if strings.TrimSpace(cfg.KubernetesAPIURL) == "" {
		return nil, fmt.Errorf("kubernetes api url is required")
	}
	token, err := readOptionalFile(cfg.KubernetesToken)
	if err != nil {
		return nil, err
	}
	client, err := newKubernetesHTTPClient(cfg.KubernetesCA)
	if err != nil {
		return nil, err
	}
	source := NewKubernetesNamespaceSource(cfg.KubernetesAPIURL, token, cfg.NamespaceSelector, cfg.Namespaces, client)
	source.fluxNS = cfg.FluxNamespace
	return source, nil
}

func NewKubernetesNamespaceSource(apiURL, token, selector string, namespaces []string, client *http.Client) *KubernetesNamespaceSource {
	if client == nil {
		client = http.DefaultClient
	}
	allowed := make(map[string]struct{}, len(namespaces))
	for _, namespace := range namespaces {
		allowed[namespace] = struct{}{}
	}
	return &KubernetesNamespaceSource{
		apiURL:   strings.TrimRight(apiURL, "/"),
		token:    strings.TrimSpace(token),
		selector: strings.TrimSpace(selector),
		allowed:  allowed,
		fluxNS:   "flux-system",
		client:   client,
	}
}

func (s *KubernetesNamespaceSource) ListNamespaces(ctx context.Context) ([]Namespace, error) {
	req, err := s.newNamespacesRequest(ctx, false)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("list namespaces failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var list namespaceList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode namespace list: %w", err)
	}
	items := make([]Namespace, 0, len(list.Items))
	for _, item := range list.Items {
		if s.allowedNamespace(item.Metadata.Name) {
			items = append(items, item)
		}
	}
	return items, nil
}

func (s *KubernetesNamespaceSource) WatchNamespaces(ctx context.Context, handle func(NamespaceEvent) error) error {
	req, err := s.newNamespacesRequest(ctx, true)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("watch namespaces failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	decoder := json.NewDecoder(bufio.NewReader(resp.Body))
	for {
		var event namespaceWatchEvent
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF || ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("decode namespace watch event: %w", err)
		}
		if !s.allowedNamespace(event.Object.Metadata.Name) {
			continue
		}
		if err := handle(NamespaceEvent{Type: event.Type, Namespace: event.Object}); err != nil {
			return err
		}
	}
}

func (s *KubernetesNamespaceSource) ListDeployments(ctx context.Context, namespace string) ([]Deployment, error) {
	endpoint := s.apiURL + "/apis/apps/v1/namespaces/" + url.PathEscape(namespace) + "/deployments"
	req, err := s.newKubernetesGET(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("list deployments failed: namespace=%s status=%d body=%s", namespace, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var list deploymentList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode deployment list: %w", err)
	}
	return list.Items, nil
}

func (s *KubernetesNamespaceSource) ListPods(ctx context.Context, namespace string) ([]Pod, error) {
	endpoint := s.apiURL + "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods"
	req, err := s.newKubernetesGET(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("list pods failed: namespace=%s status=%d body=%s", namespace, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var list podList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode pod list: %w", err)
	}
	return list.Items, nil
}

func (s *KubernetesNamespaceSource) ListIngresses(ctx context.Context, namespace string) ([]Ingress, error) {
	endpoint := s.apiURL + "/apis/networking.k8s.io/v1/namespaces/" + url.PathEscape(namespace) + "/ingresses"
	req, err := s.newKubernetesGET(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("list ingresses failed: namespace=%s status=%d body=%s", namespace, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var list ingressList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode ingress list: %w", err)
	}
	return list.Items, nil
}

func (s *KubernetesNamespaceSource) ListEvents(ctx context.Context, namespace string) ([]KubernetesEvent, error) {
	endpoint := s.apiURL + "/api/v1/namespaces/" + url.PathEscape(namespace) + "/events"
	req, err := s.newKubernetesGET(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("list events failed: namespace=%s status=%d body=%s", namespace, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var list eventList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode event list: %w", err)
	}
	return list.Items, nil
}

func (s *KubernetesNamespaceSource) ListFluxKustomizations(ctx context.Context, namespace string) ([]FluxKustomization, error) {
	endpoint := s.apiURL + "/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/" + url.PathEscape(namespace) + "/kustomizations"
	req, err := s.newKubernetesGET(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("list flux kustomizations failed: namespace=%s status=%d body=%s", namespace, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var list fluxKustomizationList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode flux kustomization list: %w", err)
	}
	return list.Items, nil
}

func (s *KubernetesNamespaceSource) ListHelmReleases(ctx context.Context, namespace string) ([]HelmRelease, error) {
	endpoint := s.apiURL + "/apis/helm.toolkit.fluxcd.io/v2/namespaces/" + url.PathEscape(namespace) + "/helmreleases"
	req, err := s.newKubernetesGET(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("list helm releases failed: namespace=%s status=%d body=%s", namespace, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var list helmReleaseList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode helm release list: %w", err)
	}
	return list.Items, nil
}

func (s *KubernetesNamespaceSource) FluxNamespace() string {
	if strings.TrimSpace(s.fluxNS) == "" {
		return "flux-system"
	}
	return s.fluxNS
}

func (s *KubernetesNamespaceSource) ListIngressControllers(ctx context.Context) ([]string, error) {
	type ingressClass struct {
		Spec struct {
			Controller string `json:"controller"`
		} `json:"spec"`
	}
	type ingressClassList struct {
		Items []ingressClass `json:"items"`
	}
	endpoint := s.apiURL + "/apis/networking.k8s.io/v1/ingressclasses"
	req, err := s.newKubernetesGET(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("list ingress classes failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var list ingressClassList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode ingress class list: %w", err)
	}
	controllers := make([]string, 0, len(list.Items))
	seen := map[string]struct{}{}
	for _, item := range list.Items {
		controller := strings.TrimSpace(item.Spec.Controller)
		if controller == "" {
			continue
		}
		if _, ok := seen[controller]; ok {
			continue
		}
		seen[controller] = struct{}{}
		controllers = append(controllers, controller)
	}
	sort.Strings(controllers)
	return controllers, nil
}

func (s *KubernetesNamespaceSource) ListCRDNames(ctx context.Context) ([]string, error) {
	type crdItem struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	type crdList struct {
		Items []crdItem `json:"items"`
	}
	endpoint := s.apiURL + "/apis/apiextensions.k8s.io/v1/customresourcedefinitions"
	req, err := s.newKubernetesGET(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("list CRDs failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var list crdList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode CRD list: %w", err)
	}
	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		if name := strings.TrimSpace(item.Metadata.Name); name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func (s *KubernetesNamespaceSource) ListStorageClasses(ctx context.Context) ([]string, error) {
	type storageClassItem struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	type storageClassList struct {
		Items []storageClassItem `json:"items"`
	}
	endpoint := s.apiURL + "/apis/storage.k8s.io/v1/storageclasses"
	req, err := s.newKubernetesGET(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("list storageclasses failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var list storageClassList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode storageclass list: %w", err)
	}
	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		if name := strings.TrimSpace(item.Metadata.Name); name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func (s *KubernetesNamespaceSource) newNamespacesRequest(ctx context.Context, watch bool) (*http.Request, error) {
	endpoint, err := url.Parse(s.apiURL + "/api/v1/namespaces")
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	if s.selector != "" {
		query.Set("labelSelector", s.selector)
	}
	if watch {
		query.Set("watch", "true")
	}
	endpoint.RawQuery = query.Encode()
	return s.newKubernetesGET(ctx, endpoint.String())
}

func (s *KubernetesNamespaceSource) newKubernetesGET(ctx context.Context, endpoint string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	return req, nil
}

func (s *KubernetesNamespaceSource) allowedNamespace(name string) bool {
	if len(s.allowed) == 0 {
		return true
	}
	_, ok := s.allowed[name]
	return ok
}

func newKubernetesHTTPClient(caPath string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if ca := strings.TrimSpace(caPath); ca != "" {
		certPool, err := x509.SystemCertPool()
		if err != nil || certPool == nil {
			certPool = x509.NewCertPool()
		}
		if pem, err := os.ReadFile(ca); err == nil {
			certPool.AppendCertsFromPEM(pem)
			transport.TLSClientConfig = &tls.Config{RootCAs: certPool, MinVersion: tls.VersionTLS12}
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read kubernetes ca file: %w", err)
		}
	}
	return &http.Client{Transport: transport}, nil
}

func readOptionalFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

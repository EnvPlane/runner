package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"envpilot/internal/domain"
)

type DeploymentStatusCollector struct {
	source WorkloadSource
}

type WorkloadStatusReport struct {
	Status      domain.EnvironmentStatus
	Message     string
	Deployments []DeploymentSummary
	Pods        []PodSummary
	Ingresses   []IngressSummary
}

type DeploymentSummary struct {
	Name      string
	Desired   int32
	Ready     int32
	Available int32
	Failed    bool
	Reason    string
}

type PodSummary struct {
	Name    string
	Phase   string
	Ready   bool
	Failed  bool
	Reason  string
	Message string
}

type IngressSummary struct {
	Name      string
	Hosts     []string
	Available bool
	Addresses []string
}

func NewDeploymentStatusCollector(source WorkloadSource) *DeploymentStatusCollector {
	return &DeploymentStatusCollector{source: source}
}

func (c *DeploymentStatusCollector) Collect(ctx context.Context, namespace string) (WorkloadStatusReport, error) {
	deployments, err := c.source.ListDeployments(ctx, namespace)
	if err != nil {
		return WorkloadStatusReport{}, err
	}
	pods, err := c.source.ListPods(ctx, namespace)
	if err != nil {
		return WorkloadStatusReport{}, err
	}
	ingresses, err := c.source.ListIngresses(ctx, namespace)
	if err != nil {
		return WorkloadStatusReport{}, err
	}
	return BuildWorkloadStatusReport(deployments, pods, ingresses), nil
}

func BuildWorkloadStatusReport(deployments []Deployment, pods []Pod, ingresses []Ingress) WorkloadStatusReport {
	deploymentSummaries := make([]DeploymentSummary, 0, len(deployments))
	podSummaries := make([]PodSummary, 0, len(pods))
	ingressSummaries := make([]IngressSummary, 0, len(ingresses))
	failed := false
	allDeploymentsReady := len(deployments) > 0
	allPodsReady := len(pods) > 0
	allIngressesAvailable := len(ingresses) > 0

	for _, deployment := range deployments {
		summary := summarizeDeployment(deployment)
		deploymentSummaries = append(deploymentSummaries, summary)
		if summary.Failed {
			failed = true
		}
		if summary.Ready < summary.Desired || summary.Available < summary.Desired {
			allDeploymentsReady = false
		}
	}

	for _, pod := range pods {
		summary := summarizePod(pod)
		podSummaries = append(podSummaries, summary)
		if summary.Failed {
			failed = true
		}
		if !summary.Ready {
			allPodsReady = false
		}
	}

	for _, ingress := range ingresses {
		summary := summarizeIngress(ingress)
		ingressSummaries = append(ingressSummaries, summary)
		if !summary.Available {
			allIngressesAvailable = false
		}
	}

	sort.Slice(deploymentSummaries, func(i, j int) bool {
		return deploymentSummaries[i].Name < deploymentSummaries[j].Name
	})
	sort.Slice(podSummaries, func(i, j int) bool {
		return podSummaries[i].Name < podSummaries[j].Name
	})
	sort.Slice(ingressSummaries, func(i, j int) bool {
		return ingressSummaries[i].Name < ingressSummaries[j].Name
	})

	status := domain.StatusCreating
	if failed {
		status = domain.StatusFailed
	} else if allDeploymentsReady && allPodsReady && allIngressesAvailable {
		status = domain.StatusReady
	}

	return WorkloadStatusReport{
		Status:      status,
		Message:     workloadStatusMessage(status, deploymentSummaries, podSummaries, ingressSummaries),
		Deployments: deploymentSummaries,
		Pods:        podSummaries,
		Ingresses:   ingressSummaries,
	}
}

func summarizeDeployment(deployment Deployment) DeploymentSummary {
	desired := int32(1)
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}
	summary := DeploymentSummary{
		Name:      deployment.Metadata.Name,
		Desired:   desired,
		Ready:     deployment.Status.ReadyReplicas,
		Available: deployment.Status.AvailableReplicas,
	}
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == "Progressing" && condition.Status == "False" {
			summary.Failed = true
			summary.Reason = firstNonEmpty(condition.Reason, condition.Message, "progressing=false")
			break
		}
		if condition.Type == "ReplicaFailure" && condition.Status == "True" {
			summary.Failed = true
			summary.Reason = firstNonEmpty(condition.Reason, condition.Message, "replica failure")
			break
		}
	}
	return summary
}

func summarizePod(pod Pod) PodSummary {
	summary := PodSummary{
		Name:  pod.Metadata.Name,
		Phase: pod.Status.Phase,
		Ready: podReady(pod),
	}
	if strings.EqualFold(pod.Status.Phase, "Failed") {
		summary.Failed = true
		summary.Reason = firstNonEmpty(pod.Status.Reason, "pod failed")
		summary.Message = pod.Status.Message
		return summary
	}

	for _, container := range pod.Status.ContainerStatuses {
		if container.State.Waiting != nil && failedWaitingReason(container.State.Waiting.Reason) {
			summary.Failed = true
			summary.Reason = container.Name + ":" + container.State.Waiting.Reason
			summary.Message = container.State.Waiting.Message
			return summary
		}
		if container.State.Terminated != nil && container.State.Terminated.ExitCode != 0 {
			summary.Failed = true
			summary.Reason = fmt.Sprintf("%s:%s:%d", container.Name, firstNonEmpty(container.State.Terminated.Reason, "terminated"), container.State.Terminated.ExitCode)
			summary.Message = container.State.Terminated.Message
			return summary
		}
	}

	return summary
}

func summarizeIngress(ingress Ingress) IngressSummary {
	hosts := make([]string, 0, len(ingress.Spec.Rules))
	for _, rule := range ingress.Spec.Rules {
		if strings.TrimSpace(rule.Host) != "" {
			hosts = append(hosts, strings.TrimSpace(rule.Host))
		}
	}
	addresses := make([]string, 0, len(ingress.Status.LoadBalancer.Ingress))
	for _, address := range ingress.Status.LoadBalancer.Ingress {
		value := firstNonEmpty(address.Hostname, address.IP)
		if value != "" {
			addresses = append(addresses, value)
		}
	}
	sort.Strings(hosts)
	sort.Strings(addresses)
	return IngressSummary{
		Name:      ingress.Metadata.Name,
		Hosts:     hosts,
		Available: len(addresses) > 0,
		Addresses: addresses,
	}
}

func podReady(pod Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == "Ready" {
			return condition.Status == "True"
		}
	}
	if len(pod.Status.ContainerStatuses) == 0 {
		return strings.EqualFold(pod.Status.Phase, "Running") || strings.EqualFold(pod.Status.Phase, "Succeeded")
	}
	for _, container := range pod.Status.ContainerStatuses {
		if !container.Ready {
			return false
		}
	}
	return true
}

func failedWaitingReason(reason string) bool {
	switch reason {
	case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError", "CreateContainerError", "InvalidImageName", "RunContainerError":
		return true
	default:
		return false
	}
}

func workloadStatusMessage(status domain.EnvironmentStatus, deployments []DeploymentSummary, pods []PodSummary, ingresses []IngressSummary) string {
	parts := []string{fmt.Sprintf("deployment status=%s", status)}
	for _, deployment := range deployments {
		item := fmt.Sprintf("deployment/%s ready=%d/%d available=%d/%d", deployment.Name, deployment.Ready, deployment.Desired, deployment.Available, deployment.Desired)
		if deployment.Failed {
			item += " failed=" + deployment.Reason
		}
		parts = append(parts, item)
	}
	for _, pod := range pods {
		item := fmt.Sprintf("pod/%s phase=%s ready=%t", pod.Name, firstNonEmpty(pod.Phase, "unknown"), pod.Ready)
		if pod.Failed {
			item += " failed=" + pod.Reason
		}
		parts = append(parts, item)
	}
	for _, ingress := range ingresses {
		item := fmt.Sprintf("ingress/%s available=%t", ingress.Name, ingress.Available)
		if len(ingress.Hosts) > 0 {
			item += " hosts=" + strings.Join(ingress.Hosts, ",")
		}
		if len(ingress.Addresses) > 0 {
			item += " addresses=" + strings.Join(ingress.Addresses, ",")
		}
		parts = append(parts, item)
	}
	return strings.Join(parts, "; ")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

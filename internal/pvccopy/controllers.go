package pvccopy

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

func consumes(value any, claim string) bool {
	switch x := value.(type) {
	case map[string]any:
		if pvc, ok := x["persistentVolumeClaim"].(map[string]any); ok && pvc["claimName"] == claim {
			return true
		}
		for _, v := range x {
			if consumes(v, claim) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if consumes(v, claim) {
				return true
			}
		}
	}
	return false
}

func controllerSafe(obj map[string]any, ref PVCRef) error {
	meta, _ := obj["metadata"].(map[string]any)
	spec, _ := obj["spec"].(map[string]any)
	status, _ := obj["status"].(map[string]any)
	if meta["namespace"] != ref.Namespace {
		return ErrUnsafe
	}
	uses := consumes(spec, ref.Name)
	// StatefulSet claim templates generate names instead of explicit PVC refs.
	if obj["kind"] == "StatefulSet" {
		templates, _ := spec["volumeClaimTemplates"].([]any)
		for _, value := range templates {
			template, _ := value.(map[string]any)
			m, _ := template["metadata"].(map[string]any)
			name, _ := m["name"].(string)
			controller, _ := meta["name"].(string)
			prefix := name + "-" + controller + "-"
			if name != "" && controller != "" && strings.HasPrefix(ref.Name, prefix) {
				if ordinal, err := strconv.ParseUint(strings.TrimPrefix(ref.Name, prefix), 10, 32); err == nil && ordinal <= 1<<31 {
					uses = true
				}
			}
		}
	}
	if !uses {
		return nil
	}
	zero := func(m map[string]any, key string, defaultValue float64) bool {
		v, exists := m[key]
		if !exists {
			return defaultValue == 0
		}
		n, ok := v.(float64)
		return ok && n == 0
	}
	safe := false
	switch obj["kind"] {
	case "Deployment", "StatefulSet", "ReplicaSet":
		safe = zero(spec, "replicas", 1) && zero(status, "replicas", 0) && zero(status, "readyReplicas", 0)
	case "Job":
		safe = spec["suspend"] == true && zero(status, "active", 0)
	case "CronJob":
		active, ok := status["active"].([]any)
		if _, exists := status["active"]; exists && !ok {
			return ErrUnsafe
		}
		safe = spec["suspend"] == true && len(active) == 0
	case "DaemonSet":
		safe = false
	default:
		return fmt.Errorf("%w: unknown source controller", ErrUnsafe)
	}
	if !safe {
		return fmt.Errorf("%w: source-consuming controller is not fenced", ErrUnsafe)
	}
	return nil
}

// CheckControllers is read-only. Missing list permission fails, never grants it.
// An owner-maintenance declaration PLUS suspended controllers still relies on
// the owner preventing arbitrary new Pod/controller creation throughout copy.
func (k *Kubectl) CheckControllers(ctx context.Context, ref PVCRef) error {
	var list struct {
		Items    []map[string]any `json:"items"`
		Metadata struct {
			Continue string `json:"continue"`
		} `json:"metadata"`
	}
	if err := k.json(ctx, []string{"get", "deployments,statefulsets,replicasets,daemonsets,jobs,cronjobs", "-n", ref.Namespace, "-o", "json", "--chunk-size=0", "--request-timeout=30s"}, nil, &list); err != nil {
		return err
	}
	if list.Metadata.Continue != "" {
		return ErrUnsafe
	}
	for _, obj := range list.Items {
		if err := controllerSafe(obj, ref); err != nil {
			return err
		}
	}
	return nil
}

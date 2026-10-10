package pvccopy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// CommandRunner is injectable for kubectl contract tests; production uses an
// argv-only OS process, never a shell or kubectl cp/tar in an image.
type CommandRunner interface {
	Run(context.Context, []string, io.Reader, io.Writer) error
}
type OSKubectl struct{ Binary string }

func (k OSKubectl) Run(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	binary := k.Binary
	if binary == "" {
		binary = "kubectl"
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin = in
	cmd.Stdout = out
	// Do not surface arbitrary command output, tokens, or data paths on errors.
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("kubectl %s failed: %w", args[0], err)
	}
	return nil
}

type Kubectl struct {
	Commands CommandRunner
	// RunnerImage must be the exact digest reference of the compatible runner
	// image deployed by the integrator, not a user-selected backup/tool image.
	RunnerImage    string
	ServiceAccount string
	// Out-of-band operator opt-in, never supplied by the plan. Restricted
	// PodSecurity namespaces reject this; we never change their policy.
	AllowRootHelpers bool
}

func (k *Kubectl) run(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	if k.Commands != nil {
		return k.Commands.Run(ctx, args, in, out)
	}
	return (OSKubectl{}).Run(ctx, args, in, out)
}
func (k *Kubectl) json(ctx context.Context, args []string, input any, out any) error {
	var in io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		in = bytes.NewReader(b)
	}
	buf := &boundedBuffer{Limit: 8 << 20}
	if err := k.run(ctx, args, in, buf); err != nil {
		return err
	}
	// Kubernetes responses include fields irrelevant to this narrow executor.
	return json.Unmarshal(buf.Bytes(), out)
}

type objectMeta struct {
	Namespace         string            `json:"namespace"`
	Name              string            `json:"name"`
	UID               string            `json:"uid"`
	DeletionTimestamp *string           `json:"deletionTimestamp"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
}
type pvcObject struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		VolumeName string `json:"volumeName"`
		VolumeMode string `json:"volumeMode"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}
type podObject struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		HostNetwork   bool   `json:"hostNetwork"`
		HostPID       bool   `json:"hostPID"`
		HostIPC       bool   `json:"hostIPC"`
		RestartPolicy string `json:"restartPolicy"`
		Deadline      int64  `json:"activeDeadlineSeconds"`
		Security      struct {
			RunAsUser          *int64  `json:"runAsUser"`
			RunAsGroup         *int64  `json:"runAsGroup"`
			RunAsNonRoot       *bool   `json:"runAsNonRoot"`
			FSGroup            *int64  `json:"fsGroup"`
			SupplementalGroups []int64 `json:"supplementalGroups"`
			SELinux            any     `json:"seLinuxOptions"`
			Seccomp            struct {
				Type string `json:"type"`
			} `json:"seccompProfile"`
		} `json:"securityContext"`
		ServiceAccount      string            `json:"serviceAccountName"`
		Automount           *bool             `json:"automountServiceAccountToken"`
		InitContainers      []json.RawMessage `json:"initContainers"`
		EphemeralContainers []json.RawMessage `json:"ephemeralContainers"`
		Containers          []struct {
			Name     string            `json:"name"`
			Image    string            `json:"image"`
			Command  []string          `json:"command"`
			Args     []string          `json:"args"`
			Env      []json.RawMessage `json:"env"`
			EnvFrom  []json.RawMessage `json:"envFrom"`
			Security struct {
				Privileged      bool   `json:"privileged"`
				AllowEscalation *bool  `json:"allowPrivilegeEscalation"`
				ReadOnlyRoot    *bool  `json:"readOnlyRootFilesystem"`
				RunAsUser       *int64 `json:"runAsUser"`
				RunAsGroup      *int64 `json:"runAsGroup"`
				RunAsNonRoot    *bool  `json:"runAsNonRoot"`
				SELinux         any    `json:"seLinuxOptions"`
				Capabilities    struct {
					Drop []string `json:"drop"`
					Add  []string `json:"add"`
				} `json:"capabilities"`
			} `json:"securityContext"`
			VolumeMounts []struct {
				Name              string `json:"name"`
				Path              string `json:"mountPath"`
				ReadOnly          bool   `json:"readOnly"`
				RecursiveReadOnly string `json:"recursiveReadOnly"`
			} `json:"volumeMounts"`
		} `json:"containers"`
		Volumes []struct {
			Name string `json:"name"`
			PVC  *struct {
				ClaimName string `json:"claimName"`
				ReadOnly  bool   `json:"readOnly"`
			} `json:"persistentVolumeClaim"`
		} `json:"volumes"`
	} `json:"spec"`
	Status struct {
		Phase      string `json:"phase"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

func (k *Kubectl) GetPVC(ctx context.Context, ref PVCRef) (PVC, error) {
	var obj pvcObject
	if err := k.json(ctx, []string{"get", "pvc", ref.Name, "-n", ref.Namespace, "-o", "json", "--request-timeout=30s"}, nil, &obj); err != nil {
		return PVC{}, err
	}
	return PVC{Ref: PVCRef{obj.Metadata.Namespace, obj.Metadata.Name, obj.Metadata.UID}, VolumeName: obj.Spec.VolumeName, Filesystem: obj.Spec.VolumeMode == "" || obj.Spec.VolumeMode == "Filesystem", Bound: obj.Status.Phase == "Bound", Pending: obj.Status.Phase == "Pending", Deleting: obj.Metadata.DeletionTimestamp != nil, OfflineDeclared: obj.Metadata.Annotations["envplane.io/pvc-copy-offline"] == "true"}, nil
}
func (k *Kubectl) ListPods(ctx context.Context, namespace string) ([]Pod, error) {
	var list struct {
		Items    []podObject `json:"items"`
		Metadata struct {
			Continue string `json:"continue"`
		} `json:"metadata"`
	}
	if err := k.json(ctx, []string{"get", "pods", "-n", namespace, "-o", "json", "--chunk-size=0", "--request-timeout=30s"}, nil, &list); err != nil {
		return nil, err
	}
	if list.Metadata.Continue != "" {
		return nil, fmt.Errorf("%w: incomplete pod listing", ErrUnsafe)
	}
	var result []Pod
	for _, obj := range list.Items {
		p := Pod{Namespace: obj.Metadata.Namespace, Name: obj.Metadata.Name, UID: obj.Metadata.UID, Phase: obj.Status.Phase}
		for _, v := range obj.Spec.Volumes {
			if v.PVC != nil {
				p.Claims = append(p.Claims, v.PVC.ClaimName)
			}
		}
		result = append(result, p)
	}
	return result, nil
}

const ownerAnnotation = "envplane.io/pvccopy-owner"

func (k *Kubectl) CreateHelper(ctx context.Context, s HelperSpec) (Helper, error) {
	return k.createHelper(ctx, s, false)
}

// PreflightHelper performs server-side dry-run of the exact helper security
// settings before the domain adapter creates a PVC. No object is persisted.
func (k *Kubectl) PreflightHelper(ctx context.Context, s HelperSpec) error {
	_, err := k.createHelper(ctx, s, true)
	return err
}

func (k *Kubectl) createHelper(ctx context.Context, s HelperSpec, dryRun bool) (Helper, error) {
	if !imageRef.MatchString(k.RunnerImage) || s.Image != k.RunnerImage || !dnsName.MatchString(k.ServiceAccount) || len(k.ServiceAccount) > 253 {
		return Helper{}, fmt.Errorf("%w: incompatible image or existing SA configuration", ErrUnsafe)
	}
	if s.Namespace != s.PVC.Namespace || !dnsName.MatchString(s.Name) || s.Owner == "" || s.Timeout <= 0 || s.Timeout > MaxCopyTimeout {
		return Helper{}, ErrUnsafe
	}
	seconds := int64(math.Ceil(s.Timeout.Seconds())) + 30
	mount := map[string]any{"name": "data", "mountPath": "/data", "readOnly": s.ReadOnly}
	if s.ReadOnly {
		mount["recursiveReadOnly"] = "Enabled"
	}
	obj := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": s.Name, "namespace": s.Namespace, "labels": map[string]string{"app.kubernetes.io/component": "pvccopy-helper"}, "annotations": map[string]string{ownerAnnotation: s.Owner}},
		"spec": map[string]any{
			"serviceAccountName": k.ServiceAccount, "automountServiceAccountToken": false, "restartPolicy": "Never", "activeDeadlineSeconds": seconds, "terminationGracePeriodSeconds": 5,
			"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 10001, "runAsGroup": 10001, "seccompProfile": map[string]string{"type": "RuntimeDefault"}},
			"containers": []any{map[string]any{"name": "copy", "image": s.Image, "imagePullPolicy": "IfNotPresent", "command": []string{"sleep"}, "args": []string{fmt.Sprint(seconds)}, "volumeMounts": []any{mount},
				"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []string{"ALL"}}},
				"resources":       map[string]any{"requests": map[string]string{"cpu": "10m", "memory": "32Mi"}, "limits": map[string]string{"cpu": "1", "memory": "256Mi"}}}},
			"volumes": []any{map[string]any{"name": "data", "persistentVolumeClaim": map[string]any{"claimName": s.PVC.Name, "readOnly": s.ReadOnly}}},
		},
	}
	if k.AllowRootHelpers {
		spec := obj["spec"].(map[string]any)
		spec["securityContext"] = map[string]any{"runAsNonRoot": false, "runAsUser": 0, "runAsGroup": 0, "seccompProfile": map[string]string{"type": "RuntimeDefault"}}
		caps := []string{"DAC_OVERRIDE"}
		if !s.ReadOnly {
			caps = append(caps, "CHOWN", "FOWNER")
		}
		container := spec["containers"].([]any)[0].(map[string]any)
		container["securityContext"].(map[string]any)["capabilities"] = map[string]any{"drop": []string{"ALL"}, "add": caps}
	}
	var response podObject
	args := []string{"create", "-f", "-", "-n", s.Namespace, "-o", "json", "--request-timeout=30s"}
	if dryRun {
		args = append(args, "--dry-run=server")
	}
	if err := k.json(ctx, args, obj, &response); err != nil {
		return Helper{}, err
	}
	h := Helper{Namespace: response.Metadata.Namespace, Name: response.Metadata.Name, UID: response.Metadata.UID, Owner: response.Metadata.Annotations[ownerAnnotation], PVC: s.PVC, ReadOnly: s.ReadOnly, Image: s.Image}
	if dryRun {
		if h.Namespace != s.Namespace || h.Name != s.Name || h.Owner != s.Owner {
			return Helper{}, ErrUnsafe
		}
		return Helper{}, k.validateHelperPod(response, h)
	}
	if h.Namespace != s.Namespace || h.Name != s.Name || h.UID == "" || h.Owner != s.Owner {
		return Helper{}, fmt.Errorf("%w: helper create identity mismatch", ErrUnsafe)
	}
	if err := k.validateHelperPod(response, h); err != nil {
		return h, err
	}
	return h, nil
}

func (k *Kubectl) getHelper(ctx context.Context, h Helper) (podObject, error) {
	obj, err := k.getOwnedHelper(ctx, h)
	if err != nil {
		return obj, err
	}
	if err := k.validateHelperPod(obj, h); err != nil {
		return obj, err
	}
	return obj, nil
}

func (k *Kubectl) getOwnedHelper(ctx context.Context, h Helper) (podObject, error) {
	if h.UID == "" || h.Owner == "" || !dnsName.MatchString(h.Name) || !dnsName.MatchString(h.Namespace) {
		return podObject{}, ErrUnsafe
	}
	var obj podObject
	if err := k.json(ctx, []string{"get", "pod", h.Name, "-n", h.Namespace, "-o", "json", "--request-timeout=30s"}, nil, &obj); err != nil {
		return obj, err
	}
	if obj.Metadata.UID != h.UID || obj.Metadata.Name != h.Name || obj.Metadata.Namespace != h.Namespace || obj.Metadata.Annotations[ownerAnnotation] != h.Owner || obj.Metadata.Labels["app.kubernetes.io/component"] != "pvccopy-helper" {
		return obj, fmt.Errorf("%w: helper ownership mismatch", ErrUnsafe)
	}
	return obj, nil
}

func (k *Kubectl) validateHelperPod(obj podObject, h Helper) error {
	s := obj.Spec
	if h.Image != k.RunnerImage || h.PVC.Namespace != h.Namespace || s.ServiceAccount != k.ServiceAccount || s.Automount == nil || *s.Automount || len(s.Containers) != 1 || len(s.InitContainers) != 0 || len(s.EphemeralContainers) != 0 || len(s.Volumes) != 1 {
		return fmt.Errorf("%w: mutated helper specification", ErrUnsafe)
	}
	c := s.Containers[0]
	v := s.Volumes[0]
	uid, nonRoot := int64(10001), true
	var caps []string
	if k.AllowRootHelpers {
		uid, nonRoot = 0, false
		caps = []string{"DAC_OVERRIDE"}
		if !h.ReadOnly {
			caps = append(caps, "CHOWN", "FOWNER")
		}
	}
	security := s.Security
	if security.RunAsUser == nil || *security.RunAsUser != uid || security.RunAsGroup == nil || *security.RunAsGroup != uid || security.RunAsNonRoot == nil || *security.RunAsNonRoot != nonRoot || security.FSGroup != nil || len(security.SupplementalGroups) != 0 || security.SELinux != nil || security.Seccomp.Type != "RuntimeDefault" || s.HostNetwork || s.HostPID || s.HostIPC || s.RestartPolicy != "Never" {
		return fmt.Errorf("%w: mutated helper pod security", ErrUnsafe)
	}
	cs := c.Security
	if cs.Privileged || cs.AllowEscalation == nil || *cs.AllowEscalation || cs.ReadOnlyRoot == nil || !*cs.ReadOnlyRoot || cs.RunAsUser != nil || cs.RunAsGroup != nil || cs.RunAsNonRoot != nil || cs.SELinux != nil || !slices.Equal(cs.Capabilities.Drop, []string{"ALL"}) || !slices.Equal(cs.Capabilities.Add, caps) || len(c.Env) != 0 || len(c.EnvFrom) != 0 {
		return fmt.Errorf("%w: mutated helper container security", ErrUnsafe)
	}
	if len(c.Args) != 1 || s.Deadline <= 0 || s.Deadline > int64(MaxCopyTimeout/time.Second)+30 {
		return ErrUnsafe
	}
	seconds, err := strconv.ParseInt(c.Args[0], 10, 64)
	if err != nil || seconds != s.Deadline {
		return ErrUnsafe
	}
	if c.Name != "copy" || c.Image != h.Image || len(c.Command) != 1 || c.Command[0] != "sleep" || len(c.VolumeMounts) != 1 || v.Name != "data" || v.PVC == nil || v.PVC.ClaimName != h.PVC.Name || v.PVC.ReadOnly != h.ReadOnly {
		return ErrUnsafe
	}
	m := c.VolumeMounts[0]
	if m.Name != "data" || m.Path != "/data" || m.ReadOnly != h.ReadOnly || (h.ReadOnly && m.RecursiveReadOnly != "Enabled") {
		return fmt.Errorf("%w: mutated helper mount", ErrUnsafe)
	}
	return nil
}
func (k *Kubectl) WaitReady(ctx context.Context, h Helper) error {
	for {
		obj, err := k.getHelper(ctx, h)
		if err != nil {
			return err
		}
		if obj.Metadata.DeletionTimestamp != nil || obj.Status.Phase == "Failed" || obj.Status.Phase == "Succeeded" {
			return fmt.Errorf("helper is not running")
		}
		for _, c := range obj.Status.Conditions {
			if obj.Status.Phase == "Running" && c.Type == "Ready" && c.Status == "True" {
				return nil
			}
		}
		t := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}
func (k *Kubectl) Exec(ctx context.Context, h Helper, args []string, in io.Reader, out io.Writer) error {
	obj, err := k.getHelper(ctx, h)
	if err != nil {
		return err
	}
	if obj.Metadata.DeletionTimestamp != nil || obj.Status.Phase != "Running" {
		return ErrUnsafe
	}
	if len(args) < 3 || args[0] != HelperBinary || !strings.HasPrefix(args[1], "pvc-") {
		return ErrUnsafe
	}
	argv := []string{"exec", "-n", h.Namespace, h.Name, "-c", "copy"}
	if in != nil {
		argv = append(argv, "-i")
	}
	argv = append(argv, "--")
	argv = append(argv, args...)
	return k.run(ctx, argv, in, out)
}
func (k *Kubectl) DeleteHelper(ctx context.Context, h Helper) error {
	if _, err := k.getOwnedHelper(ctx, h); err != nil {
		return err
	}
	// Raw DELETE is required: ordinary kubectl delete does NOT provide the
	// UID precondition needed to protect same-name replacement objects.
	body := map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]string{"uid": h.UID}, "gracePeriodSeconds": 5, "propagationPolicy": "Background"}
	b, _ := json.Marshal(body)
	return k.run(ctx, []string{"delete", "--raw", "/api/v1/namespaces/" + h.Namespace + "/pods/" + h.Name, "-f", "-", "--request-timeout=5s"}, bytes.NewReader(b), io.Discard)
}

var _ Transport = (*Kubectl)(nil)

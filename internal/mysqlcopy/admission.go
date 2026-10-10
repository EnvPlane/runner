package mysqlcopy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/envplane/runner/internal/pvccopy"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// AdmissionFenceRef comes only from the trusted reviewed onboarding receipt,
// never from a client/job. Digests bind the full policy and binding specs, which
// must constrain ALL RunnerSA Pod creates in this source namespace to the
// approved no-PVC SQL helper (and reviewed filesystem exceptions, if any).
// Installation/approval is external; this package performs read-only checks.
type AdmissionFenceRef struct {
	Namespace                                  string
	PolicyName, PolicyUID, PolicySpecSHA256    string
	BindingName, BindingUID, BindingSpecSHA256 string
}

func specDigest(o map[string]any) string {
	b, _ := json.Marshal(nested(o, "spec"))
	return hashString(b)
}
func (d *NativeDriver) requireAdmission(ctx context.Context, p Plan) error {
	var approved *AdmissionFenceRef
	for i := range d.cfg.SourceAdmissionFences {
		f := &d.cfg.SourceAdmissionFences[i]
		if f.Namespace == p.Source.PVC.Namespace {
			if approved != nil {
				return ErrUnsafe
			}
			approved = f
		}
	}
	if approved == nil || !name.MatchString(approved.PolicyName) || !name.MatchString(approved.BindingName) || approved.PolicyUID == "" || approved.BindingUID == "" || len(approved.PolicySpecSHA256) != 64 || len(approved.BindingSpecSHA256) != 64 {
		return ErrUnsafe
	}
	policy, err := d.json(ctx, []string{"get", "validatingadmissionpolicy", approved.PolicyName, "-o", "json"}, nil)
	if err != nil || str(policy, "metadata", "uid") != approved.PolicyUID || nested(policy, "metadata", "deletionTimestamp") != nil || specDigest(policy) != approved.PolicySpecSHA256 || str(policy, "spec", "failurePolicy") != "Fail" || nested(policy, "spec", "paramKind") != nil {
		return ErrUnsafe
	}
	if len(arr(policy, "status", "typeChecking", "expressionWarnings")) != 0 {
		return ErrUnsafe
	}
	if observed := nested(policy, "status", "observedGeneration"); observed != nil && observed != nested(policy, "metadata", "generation") {
		return ErrUnsafe
	}
	if nested(policy, "status", "typeChecking") == nil || num(policy, "metadata", "generation") < 1 || num(policy, "status", "observedGeneration") != num(policy, "metadata", "generation") {
		return ErrUnsafe
	}
	binding, err := d.json(ctx, []string{"get", "validatingadmissionpolicybinding", approved.BindingName, "-o", "json"}, nil)
	if err != nil || str(binding, "metadata", "uid") != approved.BindingUID || nested(binding, "metadata", "deletionTimestamp") != nil || specDigest(binding) != approved.BindingSpecSHA256 || str(binding, "spec", "policyName") != approved.PolicyName {
		return ErrUnsafe
	}
	deny := false
	for _, v := range arr(binding, "spec", "validationActions") {
		if v == "Deny" {
			deny = true
		}
	}
	if !deny {
		return ErrUnsafe
	}
	if d.admissionProven {
		return nil
	}
	if err := d.probeAdmission(ctx, p, *approved, policy); err != nil {
		return err
	}
	d.admissionProven = true
	return nil
}

var fenceDenialMessage = regexp.MustCompile(`^envplane (?:MySQL|PVC) copy source fence \[sha256:[0-9a-f]{64}\]$`)

type probeOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *probeOutput) Write(p []byte) (int, error) {
	if b.overflow || len(p) > (16<<10)-b.Len() {
		b.overflow = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}
func (d *NativeDriver) probeAdmission(ctx context.Context, p Plan, ref AdmissionFenceRef, policy map[string]any) error {
	message := ""
	for _, validation := range arr(policy, "spec", "validations") {
		s := str(asMap(validation), "message")
		if fenceDenialMessage.MatchString(s) {
			if message != "" && message != s {
				return ErrUnsafe
			}
			message = s
		}
	}
	if message == "" {
		return ErrUnsafe
	}
	expected := fmt.Sprintf("ValidatingAdmissionPolicy '%s' with binding '%s' denied request: %s", ref.PolicyName, ref.BindingName, message)
	pod := map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": SourceClientHelperName(p.Source.PVC), "namespace": p.Source.PVC.Namespace, "labels": labels(p), "annotations": map[string]any{"envplane.io/mysql-copy-digest": d.planDigest}}, "spec": helperSpec(p, d.cfg.ServiceAccount, false, false, "")}
	args := []string{"create", "--dry-run=server", "-f", "-", "-n", p.Source.PVC.Namespace, "-o", "json", "--request-timeout=15s"}
	// Positive admission proves the intended helper is actually permitted. This
	// does not create/schedule a Pod or read a Secret/PVC through a mount.
	positive, err := d.json(ctx, args, pod)
	if err != nil || !podMatches(asMap(pod["spec"]), asMap(positive["spec"])) {
		return ErrUnsafe
	}
	for _, kind := range []string{"secret", "pvc"} {
		b, _ := json.Marshal(pod)
		var challenge map[string]any
		if json.Unmarshal(b, &challenge) != nil {
			return ErrUnsafe
		}
		spec := asMap(challenge["spec"])
		volume := map[string]any{"name": "unsafe-challenge"}
		if kind == "secret" {
			volume["secret"] = map[string]any{"secretName": "unapproved-mysqlcopy-challenge"}
		} else {
			volume["persistentVolumeClaim"] = map[string]any{"claimName": p.Source.PVC.Name, "readOnly": true}
		}
		spec["volumes"] = append(arr(spec, "volumes"), volume)
		container := asMap(arr(spec, "containers")[0])
		container["volumeMounts"] = append(arr(container, "volumeMounts"), map[string]any{"name": "unsafe-challenge", "mountPath": "/unsafe-challenge", "readOnly": true})
		input, err := json.Marshal(challenge)
		if err != nil {
			return ErrUnsafe
		}
		probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		output := &probeOutput{}
		if d.cfg.Commands != nil {
			runner, ok := d.cfg.Commands.(pvccopy.AdmissionFenceProbeRunner)
			if !ok {
				cancel()
				return ErrUnsafe
			}
			err = runner.RunAdmissionFenceProbe(probeCtx, args, bytes.NewReader(input), output)
		} else {
			cmd := exec.CommandContext(probeCtx, "kubectl", args...)
			cmd.Stdin = bytes.NewReader(input)
			cmd.Stdout = io.Discard
			cmd.Stderr = output
			cmd.WaitDelay = 2 * time.Second
			err = cmd.Run()
		}
		canceled := probeCtx.Err() != nil
		cancel()
		denied := false
		for _, line := range strings.Split(output.String(), "\n") {
			if strings.HasSuffix(strings.TrimSpace(line), expected) {
				denied = true
			}
		}
		if err == nil || canceled || output.overflow || !denied {
			return ErrUnsafe
		}
	}
	return nil
}

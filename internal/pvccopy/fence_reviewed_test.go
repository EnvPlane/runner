package pvccopy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/envplane/contracts/domain"
	"testing"
)

func TestReviewedAdmissionFencePinsUIDSpecAndSource(t *testing.T) {
	p := fenceTestPlan(t)
	for _, scenario := range []string{"exact", "policy-uid", "spec-sha", "source-name", "stale-generation", "duplicate-namespace"} {
		t.Run(scenario, func(t *testing.T) {
			policy, binding := fenceTestObjects(t, p)
			commands := &fenceTestCommands{t: t, policy: policy, binding: binding}
			hash := func(v any) string { b, _ := json.Marshal(v); h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
			name := policy["metadata"].(map[string]any)["name"].(string)
			refs := []ReviewedAdmissionFenceRef{}
			for _, namespace := range p.AllowedSourceNamespaces {
				refs = append(refs, ReviewedAdmissionFenceRef{Namespace: namespace, PolicyName: name, PolicyUID: "uid-policy", PolicySpecSHA256: hash(policy["spec"]), BindingName: name, BindingUID: "uid-policy", BindingSpecSHA256: hash(binding["spec"])})
			}
			plan := p
			switch scenario {
			case "policy-uid":
				refs[0].PolicyUID = "replaced-uid"
			case "spec-sha":
				refs[0].PolicySpecSHA256 = hash("wrong")
			case "source-name":
				plan.Items = append([]domain.PVCCopyItem(nil), p.Items...)
				plan.Items[0].SourceName = "unreviewed-source"
				plan.Digest, _ = plan.CanonicalDigest()
			case "stale-generation":
				policy["status"].(map[string]any)["observedGeneration"] = float64(2)
			case "duplicate-namespace":
				refs = append(refs, refs[0])
			}
			k := &Kubectl{Commands: commands}
			err := k.RequireReviewedAdmissionFence(context.Background(), plan, "runner-ns", "runtime-runner", refs)
			if (err == nil) != (scenario == "exact") {
				t.Fatalf("reviewed fence authority acceptance: %v", err)
			}
			if scenario == "exact" && commands.probes != 6 {
				t.Fatal("missing exact effective source probes")
			}
		})
	}
}

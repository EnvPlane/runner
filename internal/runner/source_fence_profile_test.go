package runner

import "testing"

func TestConfiguredPVCSourceFencesFailClosed(t *testing.T) {
	for _, raw := range []string{"[]", "null", `[{"Namespace":"source","Unknown":"x"}]`, `[] {}`} {
		if _, present, err := configuredPVCSourceFences(raw); err == nil || !present {
			t.Fatal("malformed configured receipt admitted")
		}
	}
	if _, present, err := configuredPVCSourceFences(""); err != nil || present {
		t.Fatal("legacy absent config changed")
	}
	refs, present, err := configuredPVCSourceFences(`[{"Namespace":"source","PolicyName":"fence","PolicyUID":"uid","PolicySpecSHA256":"hash","BindingName":"fence","BindingUID":"buid","BindingSpecSHA256":"bhash"}]`)
	if err != nil || !present || len(refs) != 1 {
		t.Fatal("trusted receipt decode failed")
	}
}

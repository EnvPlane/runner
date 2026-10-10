package mysqlcopy

import (
	"strings"
	"testing"
)

func TestOnlyDynamicTableCounterNormalized(t *testing.T) {
	base := "CREATE TABLE `records` (`id` int AUTO_INCREMENT PRIMARY KEY, `s` text DEFAULT 'AUTO_INCREMENT=123') ENGINE=InnoDB AUTO_INCREMENT=42 COMMENT='AUTO_INCREMENT=999';"
	other := strings.Replace(base, "AUTO_INCREMENT=42", "AUTO_INCREMENT=456", 1)
	if normalizeSchema(base) != normalizeSchema(other) {
		t.Fatal("dynamic counter compared")
	}
	for _, bad := range []string{strings.Replace(base, "AUTO_INCREMENT=123", "AUTO_INCREMENT=456", 1), strings.Replace(base, "`id` int", "`id` bigint", 1), strings.Replace(base, "PRIMARY KEY", "UNIQUE KEY", 1), strings.Replace(base, "AUTO_INCREMENT=999", "AUTO_INCREMENT=123", 1)} {
		if normalizeSchema(base) == normalizeSchema(bad) {
			t.Fatal("semantic schema change erased")
		}
	}
	a := newTracker(testPlan())
	b := newTracker(testPlan())
	if _, err := a.Write([]byte(base + "\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte(other + "\n")); err != nil {
		t.Fatal(err)
	}
	pa, err := a.Finish()
	if err != nil {
		t.Fatal(err)
	}
	pb, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if !sameProof(pa, pb) || pa.SHA256 == pb.SHA256 || pa.Bytes == pb.Bytes {
		t.Fatal("logical normalization lost raw transfer audit")
	}
}

package mysqlcopy

import (
	"io"
	"strings"
	"testing"
)

func TestUnvalidatedClientCommandNeverForwarded(t *testing.T) {
	tr := newTracker(testPlan())
	r := validateReader(strings.NewReader("\\! echo unsafe\nSELECT 1;"), tr)
	b := make([]byte, 1024)
	if n, err := r.Read(b); err == nil || n != 0 {
		t.Fatal("client command reached mysql")
	}
	tr = newTracker(testPlan())
	r = validateReader(strings.NewReader(fixtureDump), tr)
	b, err := io.ReadAll(r)
	if err != nil || string(b) != fixtureDump {
		t.Fatal("valid stream bytes changed", err)
	}
}

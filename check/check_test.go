package check

import (
	"os/exec"
	"testing"
)

func TestRun(t *testing.T) {
	if _, err := exec.LookPath("gometalinter"); err != nil {
		t.Skip("gometalinter is required to run the external checker integration test")
	}

	cr, err := Run("testdata/testrepo@v0.1.0", false)
	if err != nil {
		t.Fatal(err)
	}

	if cr.Issues != 2 {
		t.Errorf("got cr.Issues = %d, want %d", cr.Issues, 2)
	}
}

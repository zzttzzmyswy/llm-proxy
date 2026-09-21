package main

import (
	"os/exec"
	"testing"
)

// The admin page's chart arithmetic is plain JavaScript, so it is unit-tested
// under node. Running it from the Go suite keeps one command sufficient for a
// full check; without node the test is skipped rather than failed, because the
// Go side of the feature does not depend on it.
func TestAdminChartLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; skipping the page's chart tests")
	}
	out, err := exec.Command(node, "--test", "admin_js_test.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node --test admin_js_test.js: %v\n%s", err, out)
	}
}

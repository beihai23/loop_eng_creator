// internal/cli/root_test.go
package cli

import (
	"testing"
)

func TestRootHelpExitsZero(t *testing.T) {
	cmd := NewRootCmd()
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--help should not error, got %v", err)
	}
}

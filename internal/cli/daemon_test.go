package cli

import "testing"

func TestDaemonHelpExitsZero(t *testing.T) {
	cmd := NewDaemonCmd()
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--help should not error, got %v", err)
	}
}

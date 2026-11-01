package main

import (
	"os"
	"testing"
)

// TestMain makes this test binary harmless as a task supervisor: task run
// starts os.Executable() task-supervise <id>, which in a test is this binary,
// and would otherwise run every test again.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "task-supervise" {
		os.Exit(0)
	}
	os.Exit(m.Run())
}

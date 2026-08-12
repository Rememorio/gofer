package buildinfo

import "testing"

func TestCurrent(t *testing.T) {
	t.Parallel()

	got := Current()
	if got.Version == "" {
		t.Fatal("Version is empty")
	}
	if got.Commit == "" {
		t.Fatal("Commit is empty")
	}
	if got.Date == "" {
		t.Fatal("Date is empty")
	}
}

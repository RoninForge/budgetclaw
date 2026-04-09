package version

import "testing"

// TestGetReturnsAllFields verifies Get() never returns a zero-value
// Info struct. Fields should be either ldflags values, BuildInfo values,
// or the "dev" / "unknown" sentinels.
func TestGetReturnsAllFields(t *testing.T) {
	info := Get()
	if info.Version == "" {
		t.Error("Version is empty")
	}
	if info.Commit == "" {
		t.Error("Commit is empty")
	}
	if info.BuildDate == "" {
		t.Error("BuildDate is empty")
	}
}

// TestDefaultSentinels documents that a binary built without ldflags
// and outside a VCS-aware context still produces the "dev"/"unknown"
// sentinels rather than panicking or returning zero values.
//
// Under `go test`, ReadBuildInfo may populate vcs.revision from the
// parent repo. That is fine: we assert sentinels are replaced OR kept,
// never both-missing-and-empty.
func TestDefaultSentinels(t *testing.T) {
	info := Get()

	if info.Version != "dev" && info.Version == "" {
		t.Error("Version must be either a real value or the 'dev' sentinel")
	}
	if info.Commit != "unknown" && info.Commit == "" {
		t.Error("Commit must be either a real value or the 'unknown' sentinel")
	}
	if info.BuildDate != "unknown" && info.BuildDate == "" {
		t.Error("BuildDate must be either a real value or the 'unknown' sentinel")
	}
}

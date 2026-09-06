package version

import "testing"

func TestUserAgent(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })
	Version = "1.2.3"
	if got := UserAgent(); got != "gascurve/1.2.3" {
		t.Fatalf("UserAgent() = %q", got)
	}
}

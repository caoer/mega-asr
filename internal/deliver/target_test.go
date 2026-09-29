package deliver

import "testing"

// A herdr client is a window of one of the host terminals whose title ends
// in ": <workspace>" for the workspace herdr reports focused.
func TestHerdrFront(t *testing.T) {
	apps := []string{"org.sample.term", "org.sample.console"}
	for _, c := range []struct {
		why                      string
		bundle, title, workspace string
		want                     bool
	}{
		{"the second host terminal", "org.sample.console", "anvil: quarry", "quarry", true},
		{"a workspace that only starts the same", "org.sample.term", "anvil: quarry-old", "quarry", false},
		{"not a host terminal", "org.sample.viewer", "anvil: quarry", "quarry", false},
		{"a plain shell window", "org.sample.term", "watch -n 2 uptime", "quarry", false},
		{"herdr not answering", "org.sample.term", "anvil: quarry", "", false},
		{"another workspace's view", "org.sample.term", "anvil: tannery", "quarry", false},
	} {
		if got := herdrFront(c.bundle, c.title, apps, c.workspace); got != c.want {
			t.Errorf("%s: herdrFront(%q, %q, %q) = %v, want %v", c.why, c.bundle, c.title, c.workspace, got, c.want)
		}
	}
}

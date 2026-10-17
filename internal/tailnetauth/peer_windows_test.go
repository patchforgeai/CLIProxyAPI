//go:build windows

package tailnetauth

import "testing"

// A process this server may not open (the System process, PID 4, needs rights a user does not
// have) is never this user's.
func TestProcessRunsAsThisUser(t *testing.T) {
	if same, err := processRunsAsThisUser(4); err != nil || same {
		t.Fatalf("the System process: %v, %v; want another user's", same, err)
	}
}

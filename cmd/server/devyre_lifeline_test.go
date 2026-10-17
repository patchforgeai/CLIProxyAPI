package main

import (
	"flag"
	"testing"
)

// devyre: supervisors find -lifeline-stdin in the help output, and it takes no value.
func TestLifelineStdinFlag(t *testing.T) {
	f := flag.CommandLine.Lookup("lifeline-stdin")
	if f == nil {
		t.Fatal("-lifeline-stdin is not registered")
	}
	if boolFlag, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !boolFlag.IsBoolFlag() {
		t.Fatal("-lifeline-stdin must be a boolean flag")
	}
	if err := f.Value.Set("maybe"); err == nil {
		t.Fatal("a non-boolean value was accepted")
	}
	if !argvEnablesBoolFlag([]string{"-lifeline-stdin", "--discover-json"}, "discover-json") {
		t.Fatal("argv scanning took --discover-json as the value of -lifeline-stdin")
	}
}

// devyre: supervisors find -password-stdin in the help output; it takes no value and never goes
// with -password.
func TestPasswordStdinFlag(t *testing.T) {
	f := flag.CommandLine.Lookup("password-stdin")
	if f == nil {
		t.Fatal("-password-stdin is not registered")
	}
	if boolFlag, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !boolFlag.IsBoolFlag() {
		t.Fatal("-password-stdin must be a boolean flag")
	}
	if name, _ := flag.UnquoteUsage(f); name != "" {
		t.Fatalf("the usage names a value (%q); the help would not list the flag alone on its line", name)
	}
	if !argvEnablesBoolFlag([]string{"-password-stdin", "--discover-json"}, "discover-json") {
		t.Fatal("argv scanning took --discover-json as the value of -password-stdin")
	}

	devyrePasswordStdin = false
	password := "from-the-command-line"
	if err := devyreApplyPasswordStdin(&password); err != nil || password != "from-the-command-line" {
		t.Fatalf("without the flag: %q, %v", password, err)
	}
	devyrePasswordStdin = true
	t.Cleanup(func() { devyrePasswordStdin = false })
	if err := devyreApplyPasswordStdin(&password); err == nil {
		t.Fatal("-password together with -password-stdin was accepted")
	}
}

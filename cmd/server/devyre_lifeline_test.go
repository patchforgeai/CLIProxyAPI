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

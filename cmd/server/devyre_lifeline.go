package main

import (
	"flag"
	"strconv"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cmd"
)

const lifelineStdinUsage = `Run as a supervised sidecar: shut down gracefully when stdin closes or reads a "shutdown" line; /keep-alive then has no idle limit`

// devyre: -lifeline-stdin lets a supervising process own the server's lifetime through stdin
// (devyre/README.md). Registered here so main.go only needs the flag in argvFlagConsumesValue.
func init() {
	flag.BoolFunc("lifeline-stdin", lifelineStdinUsage, func(value string) error {
		enabled, errParse := strconv.ParseBool(value)
		if errParse != nil {
			return errParse
		}
		cmd.SetStdinLifeline(enabled)
		return nil
	})
}

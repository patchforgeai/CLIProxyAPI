package main

import (
	"errors"
	"flag"
	"strconv"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cmd"
)

const lifelineStdinUsage = `Run as a supervised sidecar: shut down gracefully when stdin closes or reads a "shutdown" line, report the listener on stdout; /keep-alive then has no idle limit and answers challenges`

const passwordStdinUsage = `Read the local management password from the first line of stdin instead of -password, which other local users may read from the process list`

// devyrePasswordStdin is -password-stdin.
var devyrePasswordStdin bool

// devyre: -lifeline-stdin lets a supervising process own the server's lifetime through stdin,
// and -password-stdin hands it the local password there (devyre/README.md). Registered here so
// main.go only needs the flags in argvFlagConsumesValue and one call after flag.Parse.
func init() {
	flag.BoolFunc("lifeline-stdin", lifelineStdinUsage, func(value string) error {
		enabled, errParse := strconv.ParseBool(value)
		if errParse != nil {
			return errParse
		}
		cmd.SetStdinLifeline(enabled)
		return nil
	})
	flag.BoolVar(&devyrePasswordStdin, "password-stdin", false, passwordStdinUsage)
}

// devyreApplyPasswordStdin sets *password from the first line of stdin under -password-stdin.
func devyreApplyPasswordStdin(password *string) error {
	if !devyrePasswordStdin {
		return nil
	}
	if *password != "" {
		return errors.New("-password and -password-stdin cannot be used together")
	}
	value, errRead := cmd.ReadStdinPassword()
	if errRead != nil {
		return errRead
	}
	*password = value
	return nil
}

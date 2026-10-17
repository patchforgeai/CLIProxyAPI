package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	log "github.com/sirupsen/logrus"
)

// LifelineShutdownLine is the line a supervising process writes to the server's stdin to ask
// for a graceful shutdown while it keeps stdin open.
const LifelineShutdownLine = "shutdown"

// lifelineInput is the supervisor's end of the lifeline: os.Stdin with -lifeline-stdin, nil
// otherwise. A package variable because the flag is process-wide, like the other start-up
// settings main applies before StartService.
var lifelineInput io.Reader

// SetStdinLifeline turns the stdin lifeline of StartService on or off (-lifeline-stdin).
//
// With the lifeline on, the server shuts down gracefully when stdin reaches EOF (the supervisor
// exited or closed it) or reads LifelineShutdownLine, and /keep-alive loses its idle limit: the
// supervisor no longer has to ping to keep the server alive, and a supervisor that stalls for a
// while does not lose it.
func SetStdinLifeline(enabled bool) {
	if enabled {
		lifelineInput = os.Stdin
		return
	}
	lifelineInput = nil
}

// devyreWithLifeline returns a context that ends when the lifeline does, and registers
// /keep-alive without its idle limit when a local password authenticates it.
func devyreWithLifeline(parent context.Context, builder *cliproxy.Builder, input io.Reader, localPassword string) (context.Context, *cliproxy.Builder) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		reason := watchLifeline(input)
		log.Infof("lifeline: %s, shutting down", reason)
		cancel()
	}()
	if localPassword != "" {
		builder = builder.WithServerOptions(api.WithDevyreKeepAliveProbe())
	}
	return ctx, builder
}

// watchLifeline blocks until input reaches EOF, fails or yields the shutdown line, and returns
// why. Other lines are ignored.
func watchLifeline(input io.Reader) string {
	reader := bufio.NewReader(input)
	for {
		line, errRead := reader.ReadSlice('\n')
		for errors.Is(errRead, bufio.ErrBufferFull) {
			// Longer than any command: skip the rest of the line.
			line = nil
			_, errRead = reader.ReadSlice('\n')
		}
		trimmed := bytes.TrimSpace(line)
		if string(trimmed) == LifelineShutdownLine {
			return "shutdown requested"
		}
		if len(trimmed) > 0 {
			log.Warn("lifeline: ignoring an unknown line on stdin")
		}
		if errRead != nil {
			if errors.Is(errRead, io.EOF) {
				return "stdin closed"
			}
			return "stdin failed: " + errRead.Error()
		}
	}
}

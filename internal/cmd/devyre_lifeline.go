package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	log "github.com/sirupsen/logrus"
)

// LifelineShutdownLine is the line a supervising process writes to the server's stdin to ask
// for a graceful shutdown while it keeps stdin open.
const LifelineShutdownLine = "shutdown"

// With the lifeline on, the server prints exactly one of these lines on stdout, the moment its
// API listener is bound (before it accepts a connection) or cannot be. Only this process can
// write to the pipe its supervisor reads, so the supervisor learns whether the port is held by
// the process it started without sending anything to a port another program may have taken.
const (
	LifelineListeningPrefix    = "lifeline: listening on "
	LifelineCannotListenPrefix = "lifeline: cannot listen on "
)

// maxStdinPasswordBytes bounds the line -password-stdin reads (the buffer of the shared reader).
const maxStdinPasswordBytes = 4096

var (
	// lifelineInput is the supervisor's end of the lifeline: stdin with -lifeline-stdin, nil
	// otherwise. A package variable because the flag is process-wide, like the other start-up
	// settings main applies before StartService.
	lifelineInput io.Reader
	// lifelineReport receives the listening line: stdout, which the supervisor reads.
	lifelineReport io.Writer = os.Stdout

	stdinOnce   sync.Once
	stdinReader *bufio.Reader
)

// sharedStdin is the one buffered reader over stdin, so the password line (-password-stdin) and
// the lifeline after it never lose bytes the other one buffered.
func sharedStdin() *bufio.Reader {
	stdinOnce.Do(func() { stdinReader = bufio.NewReaderSize(os.Stdin, maxStdinPasswordBytes) })
	return stdinReader
}

// SetStdinLifeline turns the stdin lifeline of StartService on or off (-lifeline-stdin).
//
// With the lifeline on, the server shuts down gracefully when stdin reaches EOF (the supervisor
// exited or closed it) or reads LifelineShutdownLine, /keep-alive loses its idle limit and
// answers the supervisor's challenges (api.LifelineProof), and the server reports its listener
// on stdout: the supervisor no longer has to ping to keep the server alive, a supervisor that
// stalls for a while does not lose it, and it never mistakes another program for its server.
func SetStdinLifeline(enabled bool) {
	if enabled {
		lifelineInput = sharedStdin()
		return
	}
	lifelineInput = nil
}

// ReadStdinPassword reads the local management password from the first line of stdin
// (-password-stdin). Unlike -password, it never appears on the command line, which other local
// users can read on some systems (/proc/<pid>/cmdline on Linux). With -lifeline-stdin the rest of
// stdin is the lifeline.
func ReadStdinPassword() (string, error) {
	return readPasswordLine(sharedStdin())
}

// readPasswordLine reads one line, without its line ending. A last line without a newline
// counts; an empty or over-long one does not.
func readPasswordLine(reader *bufio.Reader) (string, error) {
	line, errRead := reader.ReadSlice('\n')
	if errors.Is(errRead, bufio.ErrBufferFull) {
		return "", fmt.Errorf("the password line on stdin is longer than %d bytes", reader.Size())
	}
	if errRead != nil && !errors.Is(errRead, io.EOF) {
		return "", fmt.Errorf("reading the password from stdin: %w", errRead)
	}
	password := strings.TrimRight(string(line), "\r\n")
	if strings.TrimSpace(password) == "" {
		if errors.Is(errRead, io.EOF) {
			return "", errors.New("stdin ended before the password line")
		}
		return "", errors.New("the password line on stdin is empty")
	}
	return password, nil
}

// devyreWithLifeline returns a context that ends when the lifeline does, has the server report
// its listener on stdout, and registers /keep-alive without its idle limit when a local password
// authenticates it.
func devyreWithLifeline(parent context.Context, builder *cliproxy.Builder, input io.Reader, localPassword string) (context.Context, *cliproxy.Builder) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		reason := watchLifeline(input)
		log.Infof("lifeline: %s, shutting down", reason)
		cancel()
	}()
	builder = builder.WithServerOptions(api.WithDevyreListenReport(reportListen))
	if localPassword != "" {
		builder = builder.WithServerOptions(api.WithDevyreKeepAliveProbe())
	}
	return ctx, builder
}

// reportListen prints the listening line (LifelineListeningPrefix, LifelineCannotListenPrefix).
func reportListen(address string, errListen error) {
	line := LifelineListeningPrefix + address
	if errListen != nil {
		// One line whatever the error says, so the supervisor reads all of it.
		reason := strings.Join(strings.Fields(errListen.Error()), " ")
		line = LifelineCannotListenPrefix + address + ": " + reason
	}
	if _, errWrite := fmt.Fprintln(lifelineReport, line); errWrite != nil {
		log.Warnf("lifeline: could not report the listener: %v", errWrite)
	}
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

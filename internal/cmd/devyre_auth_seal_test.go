package cmd

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestReadAuthFilesKeyLine(t *testing.T) {
	key := bytes.Repeat([]byte{0xa5}, 32)
	encoded := base64.RawURLEncoding.EncodeToString(key)
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{name: "a line", input: encoded + "\n"},
		{name: "CRLF", input: encoded + "\r\n"},
		{name: "the last line without a newline", input: encoded},
		{name: "empty stdin", input: "", wantErr: "stdin ended before the key line"},
		{name: "an empty line", input: "\nshutdown\n", wantErr: "the key line on stdin is empty"},
		{name: "16 bytes", input: base64.RawURLEncoding.EncodeToString(key[:16]) + "\n", wantErr: "not 32 bytes in base64url"},
		{name: "padded base64", input: base64.URLEncoding.EncodeToString(key) + "\n", wantErr: "not 32 bytes in base64url"},
		{name: "standard base64", input: strings.ReplaceAll(strings.ReplaceAll(encoded, "-", "+"), "_", "/") + "+/\n", wantErr: "not 32 bytes in base64url"},
		{name: "not base64", input: "not a key, but a secret-looking line\n", wantErr: "not 32 bytes in base64url"},
		{name: "over-long", input: strings.Repeat("x", maxStdinPasswordBytes+1) + "\n", wantErr: "longer than 4096 bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readAuthFilesKeyLine(bufio.NewReaderSize(strings.NewReader(tt.input), maxStdinPasswordBytes))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("readAuthFilesKeyLine() = %x, %v; want an error with %q", got, err, tt.wantErr)
				}
				// The error never shows the line it read.
				if line := strings.TrimSpace(tt.input); len(line) > 8 && len(line) < 200 && strings.Contains(err.Error(), line) {
					t.Fatalf("the error shows the line: %v", err)
				}
				return
			}
			if err != nil || !bytes.Equal(got, key) {
				t.Fatalf("readAuthFilesKeyLine() = %x, %v; want %x", got, err, key)
			}
		})
	}
}

// The password, the key and a shutdown line written at once: each reader gets its line, and the
// lifeline still gets the shutdown line they buffered.
func TestTheKeyLineFollowsThePasswordAndLeavesTheLifelineIntact(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	input := "s3cret\n" + base64.RawURLEncoding.EncodeToString(key) + "\nshutdown\n"
	shared := bufio.NewReaderSize(strings.NewReader(input), maxStdinPasswordBytes)
	if got, err := readPasswordLine(shared); err != nil || got != "s3cret" {
		t.Fatalf("readPasswordLine() = %q, %v", got, err)
	}
	if got, err := readAuthFilesKeyLine(shared); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("readAuthFilesKeyLine() = %x, %v", got, err)
	}
	if got := watchLifeline(shared); got != "shutdown requested" {
		t.Fatalf("watchLifeline() = %q, want shutdown requested", got)
	}
}

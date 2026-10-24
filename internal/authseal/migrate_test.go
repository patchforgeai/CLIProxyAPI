package authseal

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSealDirSealsThePlaintextFilesAndLeavesTheRest(t *testing.T) {
	dir := t.TempDir()
	plaintext := map[string]string{
		"claude-ada@example.com.json":                     tokenJSON,
		filepath.Join("nested", "codex-lin@example.json"): `{"type":"codex","access_token":"secret"}`,
	}
	for name, content := range plaintext {
		writeTestFile(t, filepath.Join(dir, name), []byte(content))
	}
	// Another key's file: never this key's to replace or remove.
	useKey(t, testKey(2))
	othersPath := filepath.Join(dir, "claude-eve@example.com.json")
	if err := WriteFile(othersPath, []byte(tokenJSON)); err != nil {
		t.Fatal(err)
	}
	useKey(t, testKey(1))
	alreadyPath := filepath.Join(dir, "claude-bo@example.com.json")
	if err := WriteFile(alreadyPath, []byte(tokenJSON)); err != nil {
		t.Fatal(err)
	}
	untouched := map[string][]byte{
		"claude-eve@example.com.json":          readTestFile(t, othersPath),
		"claude-bo@example.com.json":           readTestFile(t, alreadyPath),
		"broken.json":                          []byte(`{"type":"claude",`),
		"empty.json":                           {},
		"routing-focus.devyre":                 []byte(`{"version":1,"focus":[]}`),
		"claude-ada.cds":                       []byte(`{"version":1}`),
		".claude-ada@example.com.json.123.tmp": []byte(`{"type":"claude"}`),
	}
	for name, content := range untouched {
		if name == "claude-eve@example.com.json" || name == "claude-bo@example.com.json" {
			continue
		}
		writeTestFile(t, filepath.Join(dir, name), content)
	}

	report, err := SealDir(dir)
	if err != nil {
		t.Fatalf("SealDir: %v", err)
	}
	if report != (SealReport{Sealed: 2, AlreadyOK: 1, Unreadable: 1, NotJSON: 1}) {
		t.Fatalf("report = %+v", report)
	}
	for name, content := range plaintext {
		path := filepath.Join(dir, name)
		if onDisk := readTestFile(t, path); !IsSealed(onDisk) || bytes.Contains(onDisk, []byte("secret")) {
			t.Fatalf("%s was not sealed: %q", name, onDisk)
		}
		// Byte for byte, so its hash in the watcher stays what it was.
		if data, errRead := ReadFile(path); errRead != nil || string(data) != content {
			t.Fatalf("%s reads back as %q, %v", name, data, errRead)
		}
	}
	for name, content := range untouched {
		if got := readTestFile(t, filepath.Join(dir, name)); !bytes.Equal(got, content) {
			t.Fatalf("%s changed: %q", name, got)
		}
	}
	if left := tempLeftovers(t, dir); len(left) != 1 {
		t.Fatalf("temporary files left: %v", left)
	}

	// Again: nothing left to seal.
	again, err := SealDir(dir)
	if err != nil || again != (SealReport{AlreadyOK: 3, Unreadable: 1, NotJSON: 1}) {
		t.Fatalf("second run = %+v, %v", again, err)
	}
}

func TestSealDirKeepsTheCreationTimes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-ada@example.com.json")
	writeTestFile(t, path, []byte(tokenJSON))
	setCreationTime(t, path, pastCreation)
	useKey(t, testKey(1))
	if report, err := SealDir(dir); err != nil || report.Sealed != 1 {
		t.Fatalf("SealDir = %+v, %v", report, err)
	}
	if got := createdAt(t, path); !got.Equal(pastCreation) {
		t.Fatalf("the creation time became %v", got)
	}
}

func TestSealDirWithoutAKeyOrAFolder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-ada@example.com.json")
	writeTestFile(t, path, []byte(tokenJSON))
	useKey(t, nil)
	if report, err := SealDir(dir); err != nil || report != (SealReport{}) {
		t.Fatalf("without a key: %+v, %v", report, err)
	}
	if got := readTestFile(t, path); string(got) != tokenJSON {
		t.Fatalf("without a key the file changed: %q", got)
	}
	useKey(t, testKey(1))
	if report, err := SealDir(filepath.Join(dir, "missing")); err != nil || report != (SealReport{}) {
		t.Fatalf("a missing folder: %+v, %v", report, err)
	}
}

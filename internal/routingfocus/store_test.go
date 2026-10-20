package routingfocus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// focusTestNow is the fixed clock of the store tests.
var focusTestNow = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// newTestStore returns an in-memory store on a clock the test moves.
func newTestStore(clock *time.Time) *Store {
	store := NewStore()
	store.now = func() time.Time { return *clock }
	return store
}

func focusIDs(focus []Focus) string {
	ids := make([]string, 0, len(focus))
	for _, f := range focus {
		ids = append(ids, f.Provider+"="+f.AuthID)
	}
	return strings.Join(ids, ",")
}

func TestStoreSetClearRetain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(t *testing.T, store *Store)
		want string
	}{
		{
			name: "one focus per provider, the newest replaces",
			run: func(t *testing.T, store *Store) {
				mustSet(t, store, Focus{AuthID: "claude-a.json", Provider: "claude"})
				mustSet(t, store, Focus{AuthID: "codex-a.json", Provider: "Codex "})
				mustSet(t, store, Focus{AuthID: "claude-b.json", Provider: " CLAUDE"})
			},
			want: "claude=claude-b.json,codex=codex-a.json",
		},
		{
			name: "a credential keeps one focus",
			run: func(t *testing.T, store *Store) {
				mustSet(t, store, Focus{AuthID: "shared.json", Provider: "claude"})
				mustSet(t, store, Focus{AuthID: "shared.json", Provider: "anthropic"})
			},
			want: "anthropic=shared.json",
		},
		{
			name: "clear by provider",
			run: func(t *testing.T, store *Store) {
				mustSet(t, store, Focus{AuthID: "claude-a.json", Provider: "claude"})
				mustSet(t, store, Focus{AuthID: "codex-a.json", Provider: "codex"})
				mustNot(t, store.ClearProvider(" Claude"))
				mustNot(t, store.ClearProvider("gemini"))
			},
			want: "codex=codex-a.json",
		},
		{
			name: "clear by auth",
			run: func(t *testing.T, store *Store) {
				mustSet(t, store, Focus{AuthID: "claude-a.json", Provider: "claude"})
				mustSet(t, store, Focus{AuthID: "codex-a.json", Provider: "codex"})
				mustNot(t, store.ClearAuth("codex-a.json"))
				mustNot(t, store.ClearAuth("codex-a.json"))
				mustNot(t, store.ClearAuth("unknown.json"))
			},
			want: "claude=claude-a.json",
		},
		{
			name: "retain drops removed credentials",
			run: func(t *testing.T, store *Store) {
				mustSet(t, store, Focus{AuthID: "claude-a.json", Provider: "claude"})
				mustSet(t, store, Focus{AuthID: "codex-a.json", Provider: "codex"})
				mustNot(t, store.Retain([]string{"claude-a.json", "other.json"}, focusTestNow))
			},
			want: "claude=claude-a.json",
		},
		{
			name: "retain keeps a focus set after the list",
			run: func(t *testing.T, store *Store) {
				mustSet(t, store, Focus{AuthID: "new.json", Provider: "claude", SetAt: focusTestNow.Add(time.Second)})
				mustNot(t, store.Retain(nil, focusTestNow))
			},
			want: "claude=new.json",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := focusTestNow
			store := newTestStore(&clock)
			tc.run(t, store)
			if got := focusIDs(store.List(clock)); got != tc.want {
				t.Fatalf("List() = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestStoreRefusesInvalidFocus(t *testing.T) {
	t.Parallel()

	clock := focusTestNow
	store := newTestStore(&clock)
	for _, f := range []Focus{
		{Provider: "claude"},
		{AuthID: "claude-a.json", Provider: " "},
		{AuthID: "claude-a.json", Provider: "claude", Until: focusTestNow},
		{AuthID: "claude-a.json", Provider: "claude", Until: focusTestNow.Add(-time.Minute)},
	} {
		if err := store.Set(f); err == nil {
			t.Fatalf("Set(%+v) succeeded", f)
		}
	}
	if !store.Empty() {
		t.Fatalf("a refused focus was stored: %+v", store.List(clock))
	}
	var nilStore *Store
	if err := nilStore.Set(Focus{AuthID: "a", Provider: "claude"}); err == nil {
		t.Fatal("Set on a nil store succeeded")
	}
	if !nilStore.Empty() || nilStore.Focused("a", clock) || nilStore.List(clock) != nil || nilStore.ClearAuth("a") != nil {
		t.Fatal("a nil store is not empty")
	}
}

func TestStoreSetAtDefaultsToNow(t *testing.T) {
	t.Parallel()

	clock := focusTestNow
	store := newTestStore(&clock)
	mustSet(t, store, Focus{AuthID: "claude-a.json", Provider: "claude"})
	explicit := focusTestNow.Add(-time.Hour)
	mustSet(t, store, Focus{AuthID: "codex-a.json", Provider: "codex", SetAt: explicit})
	list := store.List(clock)
	if len(list) != 2 || !list[0].SetAt.Equal(focusTestNow) || !list[1].SetAt.Equal(explicit) {
		t.Fatalf("List() = %+v", list)
	}
}

func TestStoreExpiry(t *testing.T) {
	t.Parallel()

	clock := focusTestNow
	store := newTestStore(&clock)
	until := focusTestNow.Add(time.Hour)
	mustSet(t, store, Focus{AuthID: "claude-a.json", Provider: "claude", Until: until})
	mustSet(t, store, Focus{AuthID: "codex-a.json", Provider: "codex"})

	if !store.Focused("claude-a.json", until.Add(-time.Nanosecond)) {
		t.Fatal("focus inert before its end")
	}
	// At its end the focus is inert everywhere, while it is still stored.
	clock = until
	if store.Focused("claude-a.json", clock) {
		t.Fatal("focus applies at its end")
	}
	if got := focusIDs(store.List(clock)); got != "codex=codex-a.json" {
		t.Fatalf("List() at the end = %s", got)
	}
	if store.count.Load() != 2 {
		t.Fatalf("stored focuses = %d, want 2 before the next write", store.count.Load())
	}
	// The next write prunes it, even one that changes nothing else.
	mustNot(t, store.ClearAuth("unknown.json"))
	if store.count.Load() != 1 {
		t.Fatalf("stored focuses = %d, want 1 after the next write", store.count.Load())
	}
	if !store.Focused("codex-a.json", clock.Add(1000*time.Hour)) {
		t.Fatal("a focus without an end expired")
	}
}

func TestStorePersistence(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "auths", FileName)
	clock := focusTestNow
	store := newTestStore(&clock)
	if err := store.Load(path); err != nil {
		t.Fatalf("Load(missing) error = %v", err)
	}
	until := focusTestNow.Add(48 * time.Hour)
	mustSet(t, store, Focus{AuthID: "claude-a.json", Provider: "claude", Until: until.In(time.FixedZone("CEST", 2*3600))})
	mustSet(t, store, Focus{AuthID: "codex-a.json", Provider: "codex", SetAt: focusTestNow.Add(-time.Minute)})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const wantFile = `{
  "version": 1,
  "focus": [
    {
      "auth_id": "claude-a.json",
      "provider": "claude",
      "until": "2026-10-21T12:00:00Z",
      "set_at": "2026-10-19T12:00:00Z"
    },
    {
      "auth_id": "codex-a.json",
      "provider": "codex",
      "until": null,
      "set_at": "2026-10-19T11:59:00Z"
    }
  ]
}
`
	if string(data) != wantFile {
		t.Fatalf("file =\n%s\nwant\n%s", data, wantFile)
	}
	if runtime.GOOS != "windows" {
		info, errStat := os.Stat(path)
		if errStat != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("file mode = %v, %v; want 0600", info.Mode().Perm(), errStat)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("auth dir holds %v, %v; want only the focus file", entries, err)
	}

	reloaded := newTestStore(&clock)
	if err = reloaded.Load(path); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(normalized(reloaded.List(clock)), normalized(store.List(clock))) {
		t.Fatalf("reloaded %+v, want %+v", reloaded.List(clock), store.List(clock))
	}

	// A load prunes what ended while the file was not read.
	clock = until
	if err = reloaded.Load(path); err != nil {
		t.Fatal(err)
	}
	if reloaded.count.Load() != 1 || focusIDs(reloaded.List(clock)) != "codex=codex-a.json" {
		t.Fatalf("after the end: %+v", reloaded.List(clock))
	}

	// Clearing every focus leaves an empty list behind.
	mustNot(t, reloaded.ClearProvider("codex"))
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file focusFile
	if err = json.Unmarshal(data, &file); err != nil || file.Version != 1 || file.Focus == nil || len(file.Focus) != 0 {
		t.Fatalf("file after clearing = %s, %v", data, err)
	}
}

// normalized drops the location and monotonic reading, which a reload cannot keep.
func normalized(focus []Focus) []Focus {
	out := make([]Focus, 0, len(focus))
	for _, f := range focus {
		f.SetAt = f.SetAt.UTC()
		if !f.Until.IsZero() {
			f.Until = f.Until.UTC()
		}
		out = append(out, f)
	}
	return out
}

func TestStoreLoadInvalidFileStartsEmpty(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, body string
	}{
		{"not JSON", "{"},
		{"unknown version", `{"version":2,"focus":[]}`},
		{"no auth ID", `{"version":1,"focus":[{"auth_id":"","provider":"claude","until":null,"set_at":"2026-10-19T12:00:00Z"}]}`},
		{"no provider", `{"version":1,"focus":[{"auth_id":"a.json","provider":"","until":null,"set_at":"2026-10-19T12:00:00Z"}]}`},
		{"two focuses for a provider", `{"version":1,"focus":[{"auth_id":"a.json","provider":"claude","until":null,"set_at":"2026-10-19T12:00:00Z"},{"auth_id":"b.json","provider":"Claude","until":null,"set_at":"2026-10-19T12:00:00Z"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), FileName)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			clock := focusTestNow
			store := newTestStore(&clock)
			mustSet(t, store, Focus{AuthID: "memory.json", Provider: "claude"})
			if err := store.Load(path); err == nil {
				t.Fatal("Load() accepted an invalid file")
			}
			if !store.Empty() {
				t.Fatalf("store after an invalid file = %+v, want empty", store.List(clock))
			}
			// The path is kept: the next write replaces the invalid file.
			mustSet(t, store, Focus{AuthID: "claude-a.json", Provider: "claude"})
			reloaded := newTestStore(&clock)
			if err := reloaded.Load(path); err != nil || focusIDs(reloaded.List(clock)) != "claude=claude-a.json" {
				t.Fatalf("reloaded %+v, %v", reloaded.List(clock), err)
			}
		})
	}
}

func TestStoreInMemoryWithoutPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	clock := focusTestNow
	store := newTestStore(&clock)
	if err := store.Load(""); err != nil {
		t.Fatal(err)
	}
	mustSet(t, store, Focus{AuthID: "claude-a.json", Provider: "claude"})
	if !store.Focused("claude-a.json", clock) {
		t.Fatal("in-memory focus not applied")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("in-memory store wrote %v, %v", entries, err)
	}
}

func TestStoreFailedWriteChangesNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	clock := focusTestNow
	store := newTestStore(&clock)
	if err := store.Load(filepath.Join(blocker, FileName)); err == nil {
		// Reading through a file is an error on some systems and "missing" on
		// others; either way the store starts empty.
		if !store.Empty() {
			t.Fatal("store not empty")
		}
	}
	if err := store.Set(Focus{AuthID: "claude-a.json", Provider: "claude"}); err == nil {
		t.Fatal("Set() succeeded without a writable file")
	}
	if !store.Empty() || store.Focused("claude-a.json", clock) {
		t.Fatal("a focus that could not be saved took effect")
	}
}

func TestStoreConcurrentUse(t *testing.T) {
	t.Parallel()

	store := NewStore()
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for index := 0; index < 200; index++ {
				authID := []string{"a", "b", "c"}[(worker+index)%3]
				switch index % 4 {
				case 0:
					_ = store.Set(Focus{AuthID: authID, Provider: []string{"claude", "codex"}[worker%2]})
				case 1:
					_ = store.ClearAuth(authID)
				default:
					_ = store.Focused(authID, time.Now())
					_ = store.List(time.Now())
				}
			}
		}(worker)
	}
	wg.Wait()
	if got := len(store.List(time.Now())); int(store.count.Load()) != got {
		t.Fatalf("count %d disagrees with %d stored focuses", store.count.Load(), got)
	}
}

func TestDefaultIsShared(t *testing.T) {
	t.Parallel()

	if Default() == nil || Default() != Default() {
		t.Fatal("Default() is not one process-wide store")
	}
}

func mustSet(t *testing.T, store *Store, f Focus) {
	t.Helper()
	if err := store.Set(f); err != nil {
		t.Fatalf("Set(%+v) error = %v", f, err)
	}
}

func mustNot(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

package auth

import (
	"testing"
	"time"
)

func TestDevyreARefreshKeepsTheEarlierEndOfTheSignIn(t *testing.T) {
	auth := &Auth{ID: "claude-a.json", Metadata: map[string]any{}}
	if _, ok := SignInEndsAt(auth); ok {
		t.Fatal("a sign-in made before ends were kept has one")
	}
	NoteSignInEnd(auth, "")
	if _, ok := auth.Metadata[MetadataSignInEndsAt]; ok {
		t.Fatal("an answer without an end gave one")
	}
	NoteSignInEnd(auth, "2026-12-20T10:00:00Z")
	if ends, ok := SignInEndsAt(auth); !ok || !ends.Equal(time.Date(2026, 12, 20, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("learned end = %v, %t", ends, ok)
	}
	NoteSignInEnd(auth, "2027-01-20T10:00:00Z")
	if got := auth.Metadata[MetadataSignInEndsAt]; got != "2026-12-20T10:00:00Z" {
		t.Fatalf("a later answer moved the end to %v", got)
	}
	NoteSignInEnd(auth, "")
	if got := auth.Metadata[MetadataSignInEndsAt]; got != "2026-12-20T10:00:00Z" {
		t.Fatalf("an answer without an end dropped the kept one: %v", got)
	}
}

// A new sign-in in place of an old one takes nothing of its end from the file it replaces.
func TestDevyreANewSignInReplacesTheEnd(t *testing.T) {
	record := &Auth{ID: "claude-a.json", Provider: "claude", Metadata: map[string]any{"email": "a@example.com"}}
	MergeExistingAuthMetadata(record, map[string]any{
		"email":              "a@example.com",
		"note":               "kept",
		MetadataSignInEndsAt: "2026-12-01T00:00:00Z",
	})
	if _, ok := record.Metadata[MetadataSignInEndsAt]; ok {
		t.Fatalf("the new sign-in took the old one's end: %v", record.Metadata)
	}
	if record.Metadata["note"] != "kept" {
		t.Fatalf("the merge dropped what it keeps: %v", record.Metadata)
	}
}

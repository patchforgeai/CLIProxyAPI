package auth

// devyre: a sign-in's life (devyre/README.md, "A sign-in's life"). Claude ends a sign-in about
// four weeks after it was made, at a time its token answer gives (refresh_token_expires_in), and
// refreshing never moves that time later. The credential keeps that end in its token file, the
// earlier of the kept and the new one at each refresh, and a new sign-in replaces it: the key is
// a token payload key (IsAuthTokenPayloadKey), which a new sign-in never takes from the file it
// replaces. A sign-in made before ends were kept has none until an answer gives one; nothing
// infers it (a file's times survive a new sign-in on Windows and macOS).

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/tokenrefresh"
)

// MetadataSignInEndsAt is the token file's key for when the sign-in ends (RFC 3339).
const MetadataSignInEndsAt = "refresh_token_expires_at"

// SignInEndsAt is when the credential's sign-in ends, and whether an answer said.
func SignInEndsAt(auth *Auth) (time.Time, bool) {
	return tokenrefresh.ParseEnd(authMetadataString(auth, MetadataSignInEndsAt))
}

// NoteSignInEnd keeps the end a refresh answered with (RFC 3339, empty when the answer gave
// none) by tokenrefresh.EndAfterRefresh: the earlier of the kept one and it.
func NoteSignInEnd(auth *Auth, answered string) {
	if auth == nil {
		return
	}
	end := tokenrefresh.EndAfterRefresh(authMetadataString(auth, MetadataSignInEndsAt), answered)
	if end == "" {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[MetadataSignInEndsAt] = end
}

package management

import (
	"strings"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// devyreAddAccountUUID lists a credential's account_uuid (a Claude account's, from its OAuth
// profile), which a supervising app used to read from the auth file itself: once the file is
// sealed (internal/authseal) only this process can read it.
func devyreAddAccountUUID(entry gin.H, auth *coreauth.Auth) {
	if entry == nil || auth == nil || auth.Metadata == nil {
		return
	}
	if uuid, ok := auth.Metadata["account_uuid"].(string); ok {
		if trimmed := strings.TrimSpace(uuid); trimmed != "" {
			entry["account_uuid"] = trimmed
		}
	}
}

// devyreAddSignIn lists the credential's sign-in, on /v8/management/credentials alone (the
// deprecated /v0 listing stays as upstream has it): `sign_in.ends_at`, when the provider ends
// the sign-in, while an answer has said (coreauth.SignInEndsAt; devyre/README.md, "A sign-in's
// life").
func devyreAddSignIn(entry gin.H, auth *coreauth.Auth) {
	if entry == nil || auth == nil {
		return
	}
	signIn := gin.H{}
	if ends, ok := coreauth.SignInEndsAt(auth); ok {
		signIn["ends_at"] = ends.UTC()
	}
	if len(signIn) > 0 {
		entry["sign_in"] = signIn
	}
}

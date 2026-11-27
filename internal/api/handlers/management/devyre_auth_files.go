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
// deprecated /v0 listing stays as upstream has it; devyre/README.md, "A sign-in's life"):
//
//   - `ends_at`: when the provider ends the sign-in, while an answer has said;
//   - `access_expires_at`: when its access token runs out, and `renews`: whether the router
//     renews it by itself (coreauth.RenewsAccessToken);
//   - `state`: `retrying` (its last refresh failed in passing, tried again at `next_try_at`) or
//     `ended` (the provider ended the sign-in), absent while it is fine, with `reason` (a
//     tokenrefresh.Kind), `answer` (the provider's, in a few words) and `since`.
func devyreAddSignIn(entry gin.H, auth *coreauth.Auth) {
	if entry == nil || auth == nil {
		return
	}
	signIn := gin.H{}
	if ends, ok := coreauth.SignInEndsAt(auth); ok {
		signIn["ends_at"] = ends.UTC()
	}
	if expires, ok := auth.AccessTokenExpirationTime(); ok && !expires.IsZero() {
		signIn["access_expires_at"] = expires.UTC()
		signIn["renews"] = coreauth.RenewsAccessToken(auth)
	}
	status := coreauth.SignInStatusOf(auth)
	if status.State != coreauth.SignInFine {
		signIn["state"] = string(status.State)
		signIn["reason"] = string(status.Reason)
		if status.Answer != "" {
			signIn["answer"] = status.Answer
		}
		if !status.Since.IsZero() {
			signIn["since"] = status.Since.UTC()
		}
		if status.State == coreauth.SignInRetrying && !status.NextTryAt.IsZero() {
			signIn["next_try_at"] = status.NextTryAt.UTC()
		}
	}
	if len(signIn) > 0 {
		entry["sign_in"] = signIn
	}
}

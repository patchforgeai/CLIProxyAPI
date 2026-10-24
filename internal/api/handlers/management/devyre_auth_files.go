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

package executor

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/antigravityaccountmodels"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// devyreAntigravityAccountModel sends a request for a Gemini API name the
// credential's account serves under another id to that id
// (internal/antigravityaccountmodels): a model family's to the variant for the
// level the request names (its thinking config, or a gemini-4-neon(low)
// suffix), else to the family's default; a name without its level suffix, and
// a retired model, to the model serving it. Google's own client sends the
// variant's id, which is what Google's Code Assist API serves.
//
// The routes are the ones the registration published for the credential, so a
// configured alias, resolved before the executor, still wins, and an excluded
// id is never reached. The request's thinking config, a suffix included, is
// kept as it is. Any other model passes unchanged.
func devyreAntigravityAccountModel(auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) cliproxyexecutor.Request {
	if auth == nil || strings.TrimSpace(req.Model) == "" {
		return req
	}
	suffix := thinking.ParseSuffix(req.Model)
	route, ok := antigravityaccountmodels.Default().Route(auth.ID, suffix.ModelName)
	if !ok {
		return req
	}
	body := opts.OriginalRequest
	if len(body) == 0 {
		body = req.Payload
	}
	level := thinking.DevyreRequestedLevel(body, opts.SourceFormat.String(), req.Model)
	upstream := route.Resolve(level)
	if upstream == "" {
		return req
	}
	if suffix.HasSuffix {
		upstream += "(" + suffix.RawSuffix + ")"
	}
	log.Debugf("antigravity account models: auth=%s model=%s level=%q -> %s", auth.ID, req.Model, level, upstream)
	req.Model = upstream
	return req
}

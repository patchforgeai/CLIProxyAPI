package executor

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/antigravityaccountmodels"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// devyreAntigravityAccountModel sends a request for a Gemini API name the
// credential's account serves under another id to that id
// (internal/antigravityaccountmodels): a model family's to the variant for the
// level the request names (its thinking config, or a gemini-4-neon(low)
// suffix), the nearest one the account serves when it lacks that level, else
// to the family's default; a name without its level suffix, and a retired
// model, to the model serving it. Google's own client sends the variant's id,
// which is what Google's Code Assist API serves.
//
// When the variant serves another level than the request names (an XHIGH
// request reaching gemini-4-carbon-high), the request's level becomes the
// variant's own, in the suffix or the body, so the router's thinking check
// takes it as Google's variant does; where even that level would be refused,
// the level is removed and the variant's id carries it. The body is
// rewritten in copies: the conductor sends the request it holds to another
// credential as it came. Otherwise the thinking config goes on as it is.
//
// The routes are the ones the registration published for the credential, so a
// configured alias, resolved before the executor, still wins, and an excluded
// id is never reached. Any other model, a Google id included, passes
// unchanged.
func devyreAntigravityAccountModel(auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	if auth == nil || strings.TrimSpace(req.Model) == "" {
		return req, opts
	}
	suffix := thinking.ParseSuffix(req.Model)
	route, ok := antigravityaccountmodels.Default().Route(auth.ID, suffix.ModelName)
	if !ok {
		return req, opts
	}
	body := opts.OriginalRequest
	if len(body) == 0 {
		body = req.Payload
	}
	format := opts.SourceFormat.String()
	level := thinking.DevyreRequestedLevel(body, format, req.Model)
	upstream := route.Resolve(level)
	if upstream == "" {
		return req, opts
	}
	rawSuffix := suffix.RawSuffix
	own := route.LevelOf(upstream)
	if level != "" && own != "" && own != level {
		set := devyreAntigravityVariantLevel(upstream, own)
		if _, fromSuffix := thinking.ParseLevelSuffix(strings.TrimSpace(rawSuffix)); suffix.HasSuffix && fromSuffix {
			rawSuffix = set
		} else {
			req.Payload = thinking.DevyreSetRequestedLevel(req.Payload, format, set)
			opts.OriginalRequest = thinking.DevyreSetRequestedLevel(opts.OriginalRequest, format, set)
		}
		log.Debugf("antigravity account models: auth=%s %s serves level %q for %q", auth.ID, upstream, set, level)
	}
	if suffix.HasSuffix && rawSuffix != "" {
		upstream += "(" + rawSuffix + ")"
	}
	log.Debugf("antigravity account models: auth=%s model=%s level=%q -> %s", auth.ID, req.Model, level, upstream)
	req.Model = upstream
	return req, opts
}

// devyreAntigravityVariantLevel is the level a request reaching variant is
// sent with: the variant's own, or none when its thinking levels, as
// registered, would refuse even that.
func devyreAntigravityVariantLevel(variant, own string) string {
	info := registry.LookupModelInfo(variant, "antigravity")
	if info != nil && info.Thinking != nil && len(info.Thinking.Levels) > 0 && !thinking.HasLevel(info.Thinking.Levels, own) {
		return ""
	}
	return own
}

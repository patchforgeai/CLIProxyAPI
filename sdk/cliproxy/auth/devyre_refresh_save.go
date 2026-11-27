package auth

// devyre: no refresh is lost (devyre/README.md, "No refresh is lost"). A refresh token is single
// use: once its refresh has been answered, the new tokens are the only ones left.

import (
	"context"
	"errors"
	"fmt"

	log "github.com/sirupsen/logrus"
)

// errRefreshSuperseded: the credential's tokens were replaced while it refreshed (a new sign-in
// through the watcher), so the refresh's tokens are not its own any more.
var errRefreshSuperseded = errors.New("the credential's tokens were replaced while it refreshed")

// rebaseRefreshedAuth decides where a refresh's tokens (updated, refreshed from base) go, given
// the credential as it is now (existing):
//
//   - existing no longer holds the refresh token the refresh spent: they are not its own any more
//     (errRefreshSuperseded), and existing's newer tokens stay;
//   - existing was registered again meanwhile (the watcher's remove and add of its file) and
//     still holds the spent one: they go to it, carried onto existing (the returned auth), where
//     upstream dropped them for a stale registration epoch and left the dead token in place;
//   - otherwise nil: they merge as they are.
func rebaseRefreshedAuth(base, existing, updated *Auth) (*Auth, error) {
	if base == nil || existing == nil || updated == nil {
		return nil, nil
	}
	spent := authRefreshToken(base)
	if spent == "" {
		return nil, nil
	}
	if authRefreshToken(existing) != spent {
		return nil, fmt.Errorf("update auth %s: %w", existing.ID, errRefreshSuperseded)
	}
	if existing.RegistrationEpoch == base.RegistrationEpoch {
		return nil, nil
	}
	rebased := existing.Clone()
	if rebased.Metadata == nil {
		rebased.Metadata = make(map[string]any)
	}
	for key := range rebased.Metadata {
		if _, kept := updated.Metadata[key]; !kept && IsAuthTokenPayloadKey(key) {
			delete(rebased.Metadata, key)
		}
	}
	for key, value := range updated.Metadata {
		if IsAuthTokenPayloadKey(key) {
			rebased.Metadata[key] = value
		}
	}
	if updated.Storage != nil {
		rebased.Storage = updated.Storage
	}
	if updated.Runtime != nil {
		rebased.Runtime = updated.Runtime
	}
	rebased.LastRefreshedAt = updated.LastRefreshedAt
	rebased.NextRefreshAfter = updated.NextRefreshAfter
	rebased.LastError = updated.LastError
	rebased.StatusMessage = updated.StatusMessage
	rebased.Unavailable = updated.Unavailable
	rebased.RefreshFailures = updated.RefreshFailures
	rebased.Status = updated.Status
	rebased.devyreSignIn = updated.devyreSignIn
	log.WithFields(log.Fields{"credential": existing.ID, "provider": existing.Provider, "step": "save"}).
		Infof("the tokens refreshed for %s go to it as registered again while it refreshed", existing.ID)
	return rebased, nil
}

// followRefresh runs what the executor does once the rotated tokens are saved (RefreshFollower)
// and merges what it changed; the saved credential when it changed nothing or failed.
func (m *Manager) followRefresh(ctx context.Context, exec ProviderExecutor, saved *Auth) *Auth {
	follower, ok := exec.(RefreshFollower)
	if !ok || saved == nil {
		return saved
	}
	followed, err := follower.AfterRefresh(ctx, saved.Clone())
	if err != nil {
		if ctx.Err() == nil {
			log.WithFields(log.Fields{"credential": saved.ID, "provider": saved.Provider, "step": "after refresh"}).Warnf("after refreshing %s: %v", saved.ID, err)
		}
		return saved
	}
	if followed == nil {
		return saved
	}
	merged, errUpdate := m.UpdatePreparedAuth(context.WithoutCancel(ctx), saved, followed)
	if errUpdate != nil || merged == nil {
		return saved
	}
	return merged
}

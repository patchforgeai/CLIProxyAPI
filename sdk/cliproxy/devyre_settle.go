package cliproxy

import coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"

// devyreSettleCredentials makes sure, as the service stops, that what the refreshes got reaches
// the token files (devyre/README.md, "No refresh is lost"): it waits a bounded while for the
// refreshes in flight (coreauth.RefreshDrainWait), then tries every save that has not landed
// once more. A restart would otherwise load refresh tokens already spent.
func (s *Service) devyreSettleCredentials() {
	if s == nil || s.coreManager == nil {
		return
	}
	s.coreManager.DrainRefreshes(coreauth.RefreshDrainWait)
	s.coreManager.FlushPendingSaves()
}

package cliproxy

// devyreSettleCredentials makes sure, as the service stops, that what the refreshes got reaches
// the token files: every save that has not landed is tried once more (devyre/README.md, "No
// refresh is lost"). A restart would otherwise load refresh tokens already spent.
func (s *Service) devyreSettleCredentials() {
	if s == nil || s.coreManager == nil {
		return
	}
	s.coreManager.FlushPendingSaves()
}

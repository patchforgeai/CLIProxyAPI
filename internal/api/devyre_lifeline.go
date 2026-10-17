package api

// WithDevyreKeepAliveProbe registers GET /keep-alive without the idle shutdown that
// WithKeepAliveEndpoint adds. The endpoint still answers only the local password, so a
// supervisor can prove that the process on the port is the one it started, while the stdin
// lifeline (internal/cmd/devyre_lifeline.go) bounds the process's life instead of the pings.
func WithDevyreKeepAliveProbe() ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.devyreKeepAliveProbe = true
	}
}

func (s *Server) devyreEnableKeepAliveProbe() {
	if s.keepAliveEnabled {
		// WithKeepAliveEndpoint already serves the route, with its idle limit.
		return
	}
	s.engine.GET("/keep-alive", s.handleKeepAlive)
}

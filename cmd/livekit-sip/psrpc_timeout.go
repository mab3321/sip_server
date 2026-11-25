package main

import (
	"os"
	"time"

	"github.com/livekit/protocol/logger"
)

const (
	// envPSRPCSelectTimeout overrides how long the SIP service waits for the
	// LiveKit server to answer control-plane requests (e.g. trunk auth).
	envPSRPCSelectTimeout = "SIP_PSRPC_SELECT_TIMEOUT"
	// defaultPSRPCSelectTimeout replaces the psrpc library default of 1s, which is
	// too short when Redis is a high-latency hop away and a pooled connection has
	// to be re-established after idling.
	defaultPSRPCSelectTimeout = 10 * time.Second
)

// psrpcSelectTimeout returns the psrpc selection timeout from the environment,
// falling back to the default when unset or invalid.
func psrpcSelectTimeout() time.Duration {
	v := os.Getenv(envPSRPCSelectTimeout)
	if v == "" {
		return defaultPSRPCSelectTimeout
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		logger.Warnw("invalid "+envPSRPCSelectTimeout+", using default", err, "default", defaultPSRPCSelectTimeout)
		return defaultPSRPCSelectTimeout
	}
	return d
}

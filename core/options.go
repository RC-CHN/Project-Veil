// Package core embeds the Veil Config5/FlightModel4/inner-v3 transport.
// Constructors accept in-memory values; node owns files and operating-system
// services. A Client can be used directly without starting a SOCKS listener.
package core

import (
	"crypto/x509"
	"errors"
	"veil.local/core/identity"
	"veil.local/core/internal/session"
	"veil.local/core/model"
	"veil.local/core/telemetry"
)

const Version = "0.5.0-engineering2"
const Protocol = "streammux-flight-v3"

type MuxLimits struct {
	Streams         int
	Opened          uint32
	ConnectionBytes uint64
	CarrierIdleMS   int
}

func (m MuxLimits) internal() session.MuxConfig {
	if m.Opened == 0 {
		m.Opened = 128
	}
	return session.MuxConfig{Streams: m.Streams, Opened: m.Opened, ConnectionBytes: m.ConnectionBytes, CarrierIdleMS: m.CarrierIdleMS}
}

type ClientOptions struct {
	ServerURL, DialAddress, Bucket                     string
	Model                                              model.Bundle
	Identity                                           identity.Identity
	Roots                                              *x509.CertPool
	MaxConnections, MaxCarriers, Window, IdleTimeoutMS int
	MaxBytes                                           uint64
	Mux                                                MuxLimits
	Events                                             telemetry.EventSink
}
type ServerOptions struct {
	Bucket                                                            string
	Model                                                             model.Bundle
	Identity                                                          identity.Identity
	ClientRoots                                                       *x509.CertPool
	ClientFingerprints, AllowCIDRs, DenyCIDRs                         []string
	DNSAddress                                                        string
	MaxConnections, MaxSessions, MaxSessionsPerIdentity               int
	MaxActiveStreams, MaxActiveStreamsPerIdentity                     int
	Window, ConnectTimeoutMS, IdleTimeoutMS, UDPMaxTargets, UDPIdleMS int
	MaxBytes                                                          uint64
	Mux                                                               MuxLimits
	Events                                                            telemetry.EventSink
}

func inputs(m model.Bundle, i identity.Identity, role identity.Role, roots *x509.CertPool) (session.MemoryInputs, error) {
	if e := m.Validate(); e != nil {
		return session.MemoryInputs{}, e
	}
	if !i.ValidFor(role) {
		return session.MemoryInputs{}, errors.New("invalid identity for role")
	}
	if roots != nil {
		roots = roots.Clone()
	}
	return session.MemoryInputs{Model: m.Bytes(), Certificate: i.Certificate(), Roots: roots}, nil
}

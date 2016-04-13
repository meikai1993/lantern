package client

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/getlantern/balancer"
	"github.com/getlantern/chained"
	"github.com/getlantern/idletiming"
	"github.com/getlantern/keyman"
	"github.com/getlantern/tlsdialer"
)

// Close connections idle for a period to avoid dangling connections.
// 1 hour is long enough to avoid interrupt normal connections but short enough
// to eliminate "too many open files" error.
var idleTimeout = 1 * time.Hour

// ForceChainedProxyAddr - If specified, all proxying will go through this address
var ForceChainedProxyAddr string

// ForceAuthToken - If specified, auth token will be forced to this
var ForceAuthToken string

type ChainedServerInfo struct {
	// Addr: the host:port of the upstream proxy server
	Addr string

	// Cert: optional PEM encoded certificate for the server. If specified,
	// server will be dialed using TLS over tcp. Otherwise, server will be
	// dialed using plain tcp. For OBFS4 proxies, this is the Base64-encoded obfs4
	// certificate.
	Cert string

	// AuthToken: the authtoken to present to the upstream server.
	AuthToken string

	// Trusted: Determines if a host can be trusted with plain HTTP traffic.
	Trusted bool

	// PluggableTransport: If specified, a pluggable transport will be used
	PluggableTransport string

	// PluggableTransportSettings: Settings for pluggable transport
	PluggableTransportSettings map[string]string
}

// Dialer creates a *balancer.Dialer backed by a chained server.
func (s *ChainedServerInfo) Dialer(deviceID string) (*balancer.Dialer, error) {
	if s.PluggableTransport != "" {
		log.Debugf("Using pluggable transport %v for server at %v", s.PluggableTransport, s.Addr)
	}

	var dial func() (net.Conn, error)
	if s.Cert == "" && !forceProxy {
		log.Error("No Cert configured for chained server, will dial with plain tcp")
		dial = func() (net.Conn, error) {
			return netd.Dial("tcp", addr)
		}
	} else {
		log.Trace("Cert configured for chained server, will dial with tls over tcp")
		cert, err := keyman.LoadCertificateFromPEMBytes([]byte(s.Cert))
		if err != nil {
			return nil, fmt.Errorf("Unable to parse certificate: %s", err)
		}
		x509cert := cert.X509()
		sessionCache := tls.NewLRUClientSessionCache(1000)
		dial = func() (net.Conn, error) {
			conn, err := tlsdialer.DialWithDialer(netd, "tcp", addr, false, &tls.Config{
				ClientSessionCache: sessionCache,
				InsecureSkipVerify: true,
			})
			if err != nil {
				return nil, err
			}
			if !forceProxy && !conn.ConnectionState().PeerCertificates[0].Equal(x509cert) {
				if errr := conn.Close(); err != nil {
					log.Debugf("Error closing chained server connection: %s", errr)
				}
				return nil, fmt.Errorf("Server's certificate didn't match expected!")
			}
			return conn, err
		}
	}

	// Is this a trusted proxy that we could use for HTTP traffic?
	var trusted string
	if s.Trusted {
		trusted = "(trusted) "
	}
	label := fmt.Sprintf("%schained proxy at %s [%v]", trusted, s.Addr, s.PluggableTransport)

	ccfg := chained.Config{
		DialServer: dial,
		Label:      label,
	}

	authToken := s.AuthToken
	if ForceAuthToken != "" {
		authToken = ForceAuthToken
	}

	ccfg.OnRequest = func(req *http.Request) {
		if authToken != "" {
			req.Header.Set("X-LANTERN-AUTH-TOKEN", authToken)
		}
		req.Header.Set("X-LANTERN-DEVICE-ID", deviceID)
	}
	d := chained.NewDialer(ccfg)

	return &balancer.Dialer{
		Label:   label,
		Trusted: s.Trusted,
		DialFN: func(network, addr string) (net.Conn, error) {
			conn, err := d(network, addr)
			if err != nil {
				return nil, err
			}

			conn = idletiming.Conn(conn, idleTimeout, func() {
				log.Debugf("Proxy connection to %s via %s idle for %v, closing", addr, conn.RemoteAddr(), idleTimeout)
				if err := conn.Close(); err != nil {
					log.Debugf("Unable to close connection: %v", err)
				}
			})

			return conn, nil
		},
		AuthToken: authToken,
	}, nil
}

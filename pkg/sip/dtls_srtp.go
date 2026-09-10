package sip

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	pdtls "github.com/pion/dtls/v3"
	prtp "github.com/pion/rtp"
	psrtp "github.com/pion/srtp/v3"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/protocol/logger"
)

// dtlsSrtpSession is deliberately a media transport: callers see the same
// RTP Session interface used by clear RTP and SDES-SRTP.
type dtlsSrtpSession struct {
	log logger.Logger
	conf *dtlsMediaConfig
	mux *dtlsMux
	ready chan struct{}
	mu sync.RWMutex
	err error
	srtp *psrtp.SessionSRTP
	srtcp *psrtp.SessionSRTCP
	dtls *pdtls.Conn
	remote net.Addr
	closeOnce sync.Once
}

func newDTLSSRTPSession(log logger.Logger, conn *udpConn, conf *dtlsMediaConfig, timeout time.Duration, remote net.Addr) *dtlsSrtpSession {
	s := &dtlsSrtpSession{log: log, conf: conf, mux: newDTLSMux(conn), ready: make(chan struct{}), remote: remote}
	go s.start(timeout)
	return s
}

func (s *dtlsSrtpSession) start(timeout time.Duration) {
	defer close(s.ready)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	verify := func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 { return errors.New("DTLS peer sent no certificate") }
		d := sha256.Sum256(raw[0]); got := make([]string, len(d))
		for i, b := range d { got[i] = fmt.Sprintf("%02X", b) }
		if !strings.EqualFold(strings.Join(got, ":"), s.conf.remoteFingerprint) { return errors.New("DTLS peer fingerprint mismatch") }
		return nil
	}
	cfg := &pdtls.Config{Certificates: []tls.Certificate{s.conf.certificate.certificate}, InsecureSkipVerify: true, VerifyPeerCertificate: verify, ClientAuth: pdtls.RequireAnyClientCert, SRTPProtectionProfiles: []pdtls.SRTPProtectionProfile{pdtls.SRTP_AEAD_AES_128_GCM, pdtls.SRTP_AES128_CM_HMAC_SHA1_80}}
	var c *pdtls.Conn
	var err error
	if s.conf.isClient { c, err = pdtls.Client(s.mux.dtls, s.remote, cfg) } else { c, err = pdtls.Server(s.mux.dtls, s.remote, cfg) }
	if err == nil { err = c.HandshakeContext(ctx) }
	if err == nil {
		profile, ok := c.SelectedSRTPProtectionProfile(); if !ok { err = errors.New("DTLS did not negotiate an SRTP profile") }
		var sp psrtp.ProtectionProfile
		if err == nil { switch profile { case pdtls.SRTP_AEAD_AES_128_GCM: sp = psrtp.ProtectionProfileAeadAes128Gcm; case pdtls.SRTP_AES128_CM_HMAC_SHA1_80: sp = psrtp.ProtectionProfileAes128CmHmacSha1_80; default: err = fmt.Errorf("unsupported DTLS-SRTP profile %v", profile) } }
		if err == nil {
			state, ok := c.ConnectionState(); if !ok { err = errors.New("DTLS connection state unavailable") }
			if err == nil {
				scfg := &psrtp.Config{Profile: sp}
				err = scfg.ExtractSessionKeysFromDTLS(&state, s.conf.isClient)
				if err == nil { s.srtp, err = psrtp.NewSessionSRTP(s.mux.srtp, scfg) }
				if err == nil { s.srtcp, err = psrtp.NewSessionSRTCP(s.mux.srtcp, scfg) }
			}
		}
	}
	s.mu.Lock(); s.dtls, s.err = c, err; s.mu.Unlock()
	if err != nil { s.log.Warnw("DTLS-SRTP handshake failed", err) } else { s.log.Infow("DTLS-SRTP handshake complete", "role", s.conf.localSetup) }
}

func (s *dtlsSrtpSession) wait() (*psrtp.SessionSRTP, error) {
	<-s.ready; s.mu.RLock(); defer s.mu.RUnlock()
	if s.err != nil { return nil, s.err }; if s.srtp == nil { return nil, io.EOF }; return s.srtp, nil
}
func (s *dtlsSrtpSession) OpenWriteStream() (rtp.WriteStream, error) { return dtlsWriteStream{s: s}, nil }
func (s *dtlsSrtpSession) AcceptStream() (rtp.ReadStream, uint32, error) { x, err := s.wait(); if err != nil { return nil, 0, err }; r, ssrc, err := x.AcceptStream(); if err != nil { return nil, 0, err }; return dtlsReadStream{r}, ssrc, nil }
func (s *dtlsSrtpSession) Close() error { s.closeOnce.Do(func(){ s.mux.Close(); s.mu.RLock(); if s.srtp != nil { _ = s.srtp.Close() }; if s.srtcp != nil { _ = s.srtcp.Close() }; if s.dtls != nil { _ = s.dtls.Close() }; s.mu.RUnlock() }); return nil }

type dtlsWriteStream struct { s *dtlsSrtpSession }
func (w dtlsWriteStream) String() string { return "DTLS-SRTPWriteStream" }
func (w dtlsWriteStream) WriteRTP(h *prtp.Header, payload []byte) (int, error) { x, err := w.s.wait(); if err != nil { return 0, err }; out, err := x.OpenWriteStream(); if err != nil { return 0, err }; return out.WriteRTP(h, payload) }
type dtlsReadStream struct { r *psrtp.ReadStreamSRTP }
func (r dtlsReadStream) ReadRTP(h *prtp.Header, payload []byte) (int, error) { n, err := r.r.Read(payload); if err != nil { return 0, err }; var p prtp.Packet; if err = p.Unmarshal(payload[:n]); err != nil { return 0, err }; *h = p.Header; return copy(payload, p.Payload), nil }

type muxPacket struct { b []byte; addr net.Addr }
type dtlsEndpoint struct { parent *dtlsMux; packets chan muxPacket; closed chan struct{}; once sync.Once }
type dtlsMux struct { conn *udpConn; dtls, srtp, srtcp *dtlsEndpoint; closed chan struct{}; once sync.Once }
func newDTLSEndpoint(m *dtlsMux) *dtlsEndpoint { return &dtlsEndpoint{parent:m, packets:make(chan muxPacket, 128), closed:make(chan struct{})} }
func newDTLSMux(c *udpConn) *dtlsMux { m:=&dtlsMux{conn:c,closed:make(chan struct{})}; m.dtls=newDTLSEndpoint(m); m.srtp=newDTLSEndpoint(m); m.srtcp=newDTLSEndpoint(m); go m.readLoop(); return m }
func (m *dtlsMux) readLoop() { b:=make([]byte, 2048); for { n,err:=m.conn.Read(b); if err != nil { m.Close(); return }; if n == 0 { continue }; p:=muxPacket{b:append([]byte(nil),b[:n]...),addr:m.conn.RemoteAddr()}; var e *dtlsEndpoint; if p.b[0]>=20 && p.b[0]<=63 { e=m.dtls } else if p.b[0]>=128 && p.b[0]<=191 { if len(p.b)>1 && p.b[1]>=192 && p.b[1]<=223 { e=m.srtcp } else { e=m.srtp } } else { continue }; select { case e.packets<-p: case <-e.closed: case <-m.closed:return } } }
func (m *dtlsMux) Close() error { m.once.Do(func(){ close(m.closed); _=m.dtls.Close(); _=m.srtp.Close(); _=m.srtcp.Close() }); return nil }
func (e *dtlsEndpoint) ReadFrom(b []byte) (int, net.Addr, error) { select { case <-e.closed:return 0,nil,io.EOF; case p:=<-e.packets:return copy(b,p.b),p.addr,nil } }
func (e *dtlsEndpoint) WriteTo(b []byte, _ net.Addr) (int,error) { select { case <-e.closed:return 0,io.EOF; default:return e.parent.conn.Write(b) } }
func (e *dtlsEndpoint) Read(b []byte) (int, error) { n, _, err := e.ReadFrom(b); return n, err }
func (e *dtlsEndpoint) Write(b []byte) (int, error) { return e.WriteTo(b, nil) }
func (e *dtlsEndpoint) RemoteAddr() net.Addr { return e.parent.conn.RemoteAddr() }
func (e *dtlsEndpoint) Close() error { e.once.Do(func(){close(e.closed)}); return nil }
func (e *dtlsEndpoint) LocalAddr() net.Addr { return e.parent.conn.LocalAddr() }
func (e *dtlsEndpoint) SetDeadline(time.Time) error { return nil }; func (e *dtlsEndpoint) SetReadDeadline(time.Time) error { return nil }; func (e *dtlsEndpoint) SetWriteDeadline(time.Time) error { return nil }

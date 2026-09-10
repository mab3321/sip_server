package sip

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	psdp "github.com/pion/sdp/v3"
)

var errDTLSSDP = errors.New("invalid DTLS-SRTP SDP")

type dtlsCertificate struct {
	certificate tls.Certificate
	fingerprint string
}

func newDTLSCertificate() (*dtlsCertificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil { return nil, err }
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil { return nil, err }
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "livekit-sip-dtls"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(24*time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	raw, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil { return nil, err }
	digest := sha256.Sum256(raw)
	parts := make([]string, len(digest))
	for i, b := range digest { parts[i] = fmt.Sprintf("%02X", b) }
	return &dtlsCertificate{certificate: tls.Certificate{Certificate: [][]byte{raw}, PrivateKey: key}, fingerprint: strings.Join(parts, ":")}, nil
}

type dtlsMediaConfig struct {
	remoteFingerprint string
	localSetup string
	isClient bool
	certificate *dtlsCertificate
}

func mediaAttribute(m *psdp.MediaDescription, key string) (string, bool) {
	for _, a := range m.Attributes { if a.Key == key { return a.Value, true } }
	return "", false
}

func sessionAttribute(s *psdp.SessionDescription, key string) (string, bool) {
	for _, a := range s.Attributes { if a.Key == key { return a.Value, true } }
	return "", false
}

func parseDTLSOffer(raw []byte, cert *dtlsCertificate) (*dtlsMediaConfig, error) {
	var s psdp.SessionDescription
	if err := s.Unmarshal(raw); err != nil { return nil, fmt.Errorf("%w: %v", errDTLSSDP, err) }
	for _, m := range s.MediaDescriptions {
		if m.MediaName.Media != "audio" || !strings.EqualFold(strings.Join(m.MediaName.Protos, "/"), "UDP/TLS/RTP/SAVPF") { continue }
		if cert == nil { return nil, fmt.Errorf("%w: DTLS-SRTP is not configured", errDTLSSDP) }
		fp, ok := mediaAttribute(m, "fingerprint"); if !ok { fp, ok = sessionAttribute(&s, "fingerprint") }
		if !ok { return nil, fmt.Errorf("%w: fingerprint missing", errDTLSSDP) }
		fields := strings.Fields(fp)
		if len(fields) != 2 || !strings.EqualFold(fields[0], "sha-256") { return nil, fmt.Errorf("%w: SHA-256 fingerprint required", errDTLSSDP) }
		v := strings.ReplaceAll(fields[1], ":", "")
		decoded, err := hex.DecodeString(v); if err != nil || len(decoded) != sha256.Size { return nil, fmt.Errorf("%w: malformed fingerprint", errDTLSSDP) }
		setup, ok := mediaAttribute(m, "setup"); if !ok { setup, ok = sessionAttribute(&s, "setup") }
		if !ok { return nil, fmt.Errorf("%w: setup missing", errDTLSSDP) }
		if _, ok = mediaAttribute(m, "rtcp-mux"); !ok { return nil, fmt.Errorf("%w: rtcp-mux required", errDTLSSDP) }
		out := &dtlsMediaConfig{remoteFingerprint: strings.ToUpper(fields[1]), certificate: cert}
		switch strings.ToLower(setup) {
		case "actpass", "active": out.localSetup, out.isClient = "passive", false
		case "passive": out.localSetup, out.isClient = "active", true
		default: return nil, fmt.Errorf("%w: unsupported setup role %q", errDTLSSDP, setup)
		}
		return out, nil
	}
	return nil, nil
}

func addDTLSAnswer(answer *psdp.SessionDescription, d *dtlsMediaConfig) error {
	for _, m := range answer.MediaDescriptions {
		if m.MediaName.Media != "audio" { continue }
		m.MediaName.Protos = []string{"UDP", "TLS", "RTP", "SAVPF"}
		attrs := m.Attributes[:0]
		for _, a := range m.Attributes { if a.Key != "crypto" && a.Key != "fingerprint" && a.Key != "setup" && a.Key != "rtcp-mux" { attrs = append(attrs, a) } }
		m.Attributes = append(attrs, psdp.Attribute{Key:"fingerprint", Value:"sha-256 " + d.certificate.fingerprint}, psdp.Attribute{Key:"setup", Value:d.localSetup}, psdp.Attribute{Key:"rtcp-mux"})
		return nil
	}
	return fmt.Errorf("%w: answer has no audio media", errDTLSSDP)
}

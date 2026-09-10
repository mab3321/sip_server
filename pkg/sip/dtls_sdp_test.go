package sip

import (
	"strings"
	"testing"

	psdp "github.com/pion/sdp/v3"
	"github.com/stretchr/testify/require"
)

const metaLikeOffer = "v=0\r\n" +
	"o=- 1 1 IN IP4 198.51.100.10\r\n" +
	"s=-\r\nt=0 0\r\n" +
	"a=fingerprint:sha-256 AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA\r\n" +
	"m=audio 40000 UDP/TLS/RTP/SAVPF 111\r\n" +
	"c=IN IP4 198.51.100.10\r\n" +
	"a=setup:actpass\r\na=rtcp-mux\r\na=rtpmap:111 opus/48000/2\r\n"

func TestParseMetaDTLSOffer(t *testing.T) {
	cert, err := newDTLSCertificate()
	require.NoError(t, err)
	c, err := parseDTLSOffer([]byte(metaLikeOffer), cert)
	require.NoError(t, err)
	require.NotNil(t, c)
	require.False(t, c.isClient)
	require.Equal(t, "passive", c.localSetup)
}

func TestParseMetaDTLSOfferRequiresMuxAndFingerprint(t *testing.T) {
	cert, err := newDTLSCertificate()
	require.NoError(t, err)
	_, err = parseDTLSOffer([]byte(strings.Replace(metaLikeOffer, "a=rtcp-mux\r\n", "", 1)), cert)
	require.ErrorIs(t, err, errDTLSSDP)
	_, err = parseDTLSOffer([]byte(strings.Replace(metaLikeOffer, "a=fingerprint:sha-256 AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA\r\n", "", 1)), cert)
	require.ErrorIs(t, err, errDTLSSDP)
}

func TestDTLSAnswerUsesSAVPF(t *testing.T) {
	cert, err := newDTLSCertificate()
	require.NoError(t, err)
	conf, err := parseDTLSOffer([]byte(metaLikeOffer), cert)
	require.NoError(t, err)
	s := &psdp.SessionDescription{MediaDescriptions: []*psdp.MediaDescription{{MediaName: psdp.MediaName{Media: "audio", Port: psdp.RangedPort{Value: 10000}, Protos: []string{"RTP", "AVP"}, Formats: []string{"111"}}, Attributes: []psdp.Attribute{{Key: "rtpmap", Value: "111 opus/48000/2"}}}}}
	require.NoError(t, addDTLSAnswer(s, conf))
	raw, err := s.Marshal(); require.NoError(t, err)
	text := string(raw)
	require.Contains(t, text, "UDP/TLS/RTP/SAVPF")
	require.Contains(t, text, "a=setup:passive")
	require.Contains(t, text, "a=rtcp-mux")
	require.Contains(t, text, "a=fingerprint:sha-256 "+cert.fingerprint)
}

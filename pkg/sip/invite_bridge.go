package sip

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/frostbyte73/core"
	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/protocol/livekit"
	lksip "github.com/livekit/protocol/sip"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/sipgo/sip"

	"github.com/livekit/sip/pkg/stats"
)

const inviteBridgeTrunkIDHeader = "X-LiveKit-Outbound-Trunk-ID"

type inviteBridgeTarget struct {
	uri       *sip.Uri
	transport Transport
	username  string
	password  string
}

func (c *inboundCall) resolveInviteBridgeTarget(ctx context.Context, transferTo string, headers map[string]string) (*inviteBridgeTarget, error) {
	rawURI := strings.TrimSpace(transferTo)
	trunkID := strings.TrimSpace(headers[inviteBridgeTrunkIDHeader])
	delete(headers, inviteBridgeTrunkIDHeader) // control metadata must never be sent to the carrier
	if trunkID == "" {
		uri, err := buildRawURI(rawURI, SIPTransportFrom(c.cc.legTr))
		if err != nil {
			return nil, err
		}
		return &inviteBridgeTarget{uri: uri, transport: c.cc.legTr}, nil
	}

	client := lksdk.NewSIPClient(c.s.conf.WsUrl, c.s.conf.ApiKey, c.s.conf.ApiSecret)
	resp, err := client.ListSIPOutboundTrunk(ctx, &livekit.ListSIPOutboundTrunkRequest{TrunkIds: []string{trunkID}})
	if err != nil {
		return nil, fmt.Errorf("load outbound trunk %q: %w", trunkID, err)
	}
	if len(resp.Items) != 1 || resp.Items[0].SipTrunkId != trunkID {
		return nil, fmt.Errorf("outbound trunk %q not found", trunkID)
	}
	trunk := resp.Items[0]
	transport := TransportFrom(trunk.Transport)

	var user string
	if strings.HasPrefix(strings.ToLower(rawURI), "tel:") {
		user = strings.TrimSpace(rawURI[len("tel:"):])
	} else if parsed, parseErr := buildRawURI(rawURI, trunk.Transport); parseErr == nil {
		user = parsed.User
	}
	if user == "" {
		return nil, errors.New("transfer destination has no user or phone number")
	}
	uri, err := buildLegacyURI(user, trunk.Address, trunk.Transport)
	if err != nil {
		return nil, fmt.Errorf("build destination from outbound trunk %q: %w", trunkID, err)
	}
	return &inviteBridgeTarget{
		uri:       uri,
		transport: transport,
		username:  trunk.AuthUsername,
		password:  trunk.AuthPassword,
	}, nil
}

// inviteBridge owns an inbound carrier dialog and the outbound dialog created
// for a non-REFER transfer. Audio and DTMF are connected directly between the
// two MediaPorts; neither leg remains a LiveKit room participant.
type inviteBridge struct {
	in       *inboundCall
	outCC    *sipOutbound
	outMedia MediaPort
	outTag   LocalTag
	to       string
	started  time.Time
	answered time.Time
	done     core.Fuse
	close    sync.Once
}

func (c *inboundCall) startInviteBridge(ctx context.Context, transferTo string, headers map[string]string) error {
	if c.s.conf.InviteDirectMediaTransfer {
		if err := c.startInviteDirectMedia(ctx, transferTo, headers); err == nil {
			return nil
		} else {
			if c.s.conf.InviteDirectMediaRequired {
				c.log().Errorw("direct-media transfer failed; anchored fallback is disabled", err)
				return fmt.Errorf("direct-media transfer required: %w", err)
			}
			c.log().Warnw("direct-media transfer failed; falling back to anchored INVITE bridge", err)
		}
	}
	return c.startAnchoredInviteBridge(ctx, transferTo, headers)
}

func (c *inboundCall) startAnchoredInviteBridge(ctx context.Context, transferTo string, headers map[string]string) error {
	if c.s.cli == nil {
		return errors.New("outbound SIP client is unavailable")
	}
	if c.inviteBridge.Load() != nil {
		return errors.New("call is already INVITE-bridged")
	}

	target, err := c.resolveInviteBridgeTarget(ctx, transferTo, headers)
	if err != nil {
		return fmt.Errorf("invalid INVITE bridge destination: %w", err)
	}
	uri := target.uri

	fromURI := c.cc.To()
	fromURI.Host = c.s.sconf.SignalingIP.String()
	fromURI.Port = c.s.conf.SIPPort
	toURI := *uri

	outTag := LocalTag(lksip.NewCallID())
	log := c.log().WithValues(
		"bridgeCallID", outTag,
		"bridgeTo", toURI.String(),
		"transferMode", "invite-bridge",
	)
	outCC := c.s.cli.newOutbound(
		log,
		outTag,
		uri,
		&sip.ToHeader{Address: toURI},
		&sip.FromHeader{Address: fromURI},
		c.s.cli.ContactURI(target.transport),
		nil,
	)
	if len(c.s.conf.OutboundRouteHeaders) != 0 {
		outCC.routeHeaders = c.s.conf.OutboundRouteHeaders
	}

	bridgeStats := &Stats{}
	outMon := c.s.mon.NewCall(stats.Outbound, fromURI.Host, toURI.Host)
	outMedia, err := NewMediaPort(log, outMon, &MediaOptions{
		IP:                   c.s.sconf.MediaIP,
		Ports:                c.s.conf.RTPPort,
		MediaTimeoutInitial:  c.s.conf.MediaTimeoutInitial,
		MediaTimeout:         c.s.conf.MediaTimeout,
		SymmetricRTP:         c.s.conf.SymmetricRTP,
		IgnoreLocalAddrInSDP: c.s.conf.IgnoreLocalAddrInSDP,
		EnableJitterBuffer:   c.jitterBuf,
		Stats:                &bridgeStats.Port,
		DrainingIdleTimeout:  c.s.conf.RTPDrainingIdleTimeout,
		DrainingDuration:     c.s.conf.RTPDrainingDuration,
		DTMFAudio:            c.s.conf.AudioDTMF,
		Codecs:               c.mediaCodecs,
		Encryption:           sdp.EncryptionNone,
	}, RoomSampleRate)
	if err != nil {
		return fmt.Errorf("create INVITE bridge media: %w", err)
	}

	bridge := &inviteBridge{
		in:       c,
		outCC:    outCC,
		outMedia: outMedia,
		outTag:   outTag,
		to:       toURI.User,
		started:  time.Now(),
	}
	c.s.cli.cmu.Lock()
	c.s.cli.bridgeCalls[outTag] = bridge
	c.s.cli.cmu.Unlock()
	cleanup := true
	defer func() {
		if cleanup {
			bridge.closeFromService(context.Background())
		}
	}()

	dialCtx, cancelDial := context.WithCancel(ctx)
	defer cancelDial()
	go func() {
		select {
		case <-c.ctx.Done():
			cancelDial()
		case <-dialCtx.Done():
		}
	}()

	offer, err := outMedia.GenerateOffer()
	if err != nil {
		return fmt.Errorf("generate INVITE bridge offer: %w", err)
	}
	answer, err := outCC.Invite(dialCtx, target.username, target.password, headers, offer, nil)
	if err != nil {
		return err
	}
	if err := outMedia.ProcessAnswer(answer); err != nil {
		return fmt.Errorf("process INVITE bridge answer: %w", err)
	}
	if err := outCC.AckInviteOK(dialCtx); err != nil {
		return fmt.Errorf("acknowledge INVITE bridge: %w", err)
	}
	bridge.answered = time.Now()
	outMedia.SetTimeout(c.s.conf.MediaTimeoutInitial, c.s.conf.MediaTimeout)

	// Stop room audio before connecting the carrier legs so no AI audio can
	// leak into the transferred conversation.
	_ = c.lkRoom.WriteOutboundAudioTo(nil)
	_ = c.lkRoom.WriteOutboundDTMFTo(nil)

	if old := c.media.WriteInboundAudioTo(msdk.NopCloser[msdk.PCM16Sample](outMedia.GetOutboundAudioWriter())); old != nil {
		_ = old.Close()
	}
	if old := outMedia.WriteInboundAudioTo(msdk.NopCloser[msdk.PCM16Sample](c.media.GetOutboundAudioWriter())); old != nil {
		_ = old.Close()
	}
	if old := c.media.WriteInboundDTMFTo(msdk.NopCloser[string](outMedia.GetOutboundDTMFWriter())); old != nil {
		_ = old.Close()
	}
	if old := outMedia.WriteInboundDTMFTo(msdk.NopCloser[string](c.media.GetOutboundDTMFWriter())); old != nil {
		_ = old.Close()
	}

	c.inviteBridge.Store(bridge)
	c.bridged.Break()
	cleanup = false
	log.Infow("INVITE bridge established; detaching call from LiveKit room")
	// Keep the transfer RPC topic alive briefly so its successful response can
	// reach LiveKit before removal of the room participant deregisters it.
	time.AfterFunc(500*time.Millisecond, func() { _ = c.lkRoom.Close() })

	go func() {
		select {
		case <-bridge.done.Watch():
		case <-outMedia.MediaTimeout():
			log.Warnw("INVITE bridge outbound media timed out", nil)
			bridge.closeFromOutbound(context.Background())
		}
	}()
	return nil
}

func (c *inboundCall) startInviteDirectMedia(ctx context.Context, transferTo string, headers map[string]string) error {
	target, err := c.resolveInviteBridgeTarget(ctx, transferTo, headers)
	if err != nil {
		return fmt.Errorf("invalid direct-media destination: %w", err)
	}
	uri := target.uri
	callerOffer := c.cc.invite.Body()
	if len(callerOffer) == 0 {
		return errors.New("caller dialog has no SDP offer")
	}
	fromURI := c.cc.To()
	fromURI.Host = c.s.sconf.SignalingIP.String()
	fromURI.Port = c.s.conf.SIPPort
	toURI := *uri
	outTag := LocalTag(lksip.NewCallID())
	log := c.log().WithValues("bridgeCallID", outTag, "bridgeTo", toURI.String(), "transferMode", "invite-direct-media")
	outCC := c.s.cli.newOutbound(log, outTag, uri, &sip.ToHeader{Address: toURI}, &sip.FromHeader{Address: fromURI}, c.s.cli.ContactURI(target.transport), nil)
	if len(c.s.conf.OutboundRouteHeaders) != 0 {
		outCC.routeHeaders = c.s.conf.OutboundRouteHeaders
	}
	bridge := &inviteBridge{in: c, outCC: outCC, outTag: outTag, to: toURI.User, started: time.Now()}
	c.s.cli.cmu.Lock()
	c.s.cli.bridgeCalls[outTag] = bridge
	c.s.cli.cmu.Unlock()
	cleanup := true
	defer func() {
		if cleanup {
			bridge.closeFromService(context.Background())
		}
	}()

	destinationSDP, err := outCC.Invite(ctx, target.username, target.password, headers, callerOffer, nil)
	if err != nil {
		return err
	}
	if err := outCC.AckInviteOK(ctx); err != nil {
		return err
	}
	if _, err := c.cc.Reinvite(ctx, destinationSDP); err != nil {
		return fmt.Errorf("re-INVITE caller for direct media: %w", err)
	}

	// RTP now flows between the carrier legs. The original MediaPort remains
	// allocated only as a signaling/lifecycle anchor, so its inactivity must
	// not terminate a healthy direct-media call.
	c.media.DisableTimeout()
	bridge.answered = time.Now()
	c.inviteBridge.Store(bridge)
	c.bridged.Break()
	cleanup = false
	log.Infow("direct-media INVITE transfer established; detaching call from LiveKit room")
	time.AfterFunc(500*time.Millisecond, func() { _ = c.lkRoom.Close() })
	return nil
}

func (b *inviteBridge) unregister() {
	b.in.s.cli.cmu.Lock()
	delete(b.in.s.cli.bridgeCalls, b.outTag)
	b.in.s.cli.cmu.Unlock()
}

func (b *inviteBridge) acceptOutboundBye(req *sip.Request, tx sip.ServerTransaction) {
	b.outCC.AcceptBye(req, tx)
	b.in.setCompletionHangupSource("destination")
	b.closeFromOutbound(context.Background())
}

func (b *inviteBridge) closeFromInbound(ctx context.Context) {
	b.close.Do(func() {
		b.unregister()
		b.outCC.Close(ctx, nil)
		if b.outMedia != nil {
			b.outMedia.Close()
		}
		b.done.Break()
	})
}

func (b *inviteBridge) closeFromOutbound(ctx context.Context) {
	b.close.Do(func() {
		b.unregister()
		if b.outMedia != nil {
			b.outMedia.Close()
		}
		b.done.Break()
		b.in.Close()
	})
}

func (b *inviteBridge) closeFromService(ctx context.Context) {
	b.closeFromInbound(ctx)
}

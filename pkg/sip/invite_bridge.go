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
	lksip "github.com/livekit/protocol/sip"
	"github.com/livekit/sipgo/sip"

	"github.com/livekit/sip/pkg/stats"
)

// inviteBridge owns an inbound carrier dialog and the outbound dialog created
// for a non-REFER transfer. Audio and DTMF are connected directly between the
// two MediaPorts; neither leg remains a LiveKit room participant.
type inviteBridge struct {
	in       *inboundCall
	outCC    *sipOutbound
	outMedia MediaPort
	outTag   LocalTag
	done     core.Fuse
	close    sync.Once
}

func (c *inboundCall) startInviteBridge(ctx context.Context, transferTo string, headers map[string]string) error {
	if c.s.cli == nil {
		return errors.New("outbound SIP client is unavailable")
	}
	if c.inviteBridge.Load() != nil {
		return errors.New("call is already INVITE-bridged")
	}

	rawURI := strings.TrimSpace(transferTo)
	uri, err := buildRawURI(rawURI, SIPTransportFrom(c.cc.legTr))
	if err != nil {
		return fmt.Errorf("invalid INVITE bridge destination: %w", err)
	}

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
		c.s.cli.ContactURI(c.cc.legTr),
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
	answer, err := outCC.Invite(dialCtx, "", "", headers, offer, nil)
	if err != nil {
		return err
	}
	if err := outMedia.ProcessAnswer(answer); err != nil {
		return fmt.Errorf("process INVITE bridge answer: %w", err)
	}
	if err := outCC.AckInviteOK(dialCtx); err != nil {
		return fmt.Errorf("acknowledge INVITE bridge: %w", err)
	}
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

func (b *inviteBridge) unregister() {
	b.in.s.cli.cmu.Lock()
	delete(b.in.s.cli.bridgeCalls, b.outTag)
	b.in.s.cli.cmu.Unlock()
}

func (b *inviteBridge) acceptOutboundBye(req *sip.Request, tx sip.ServerTransaction) {
	b.outCC.AcceptBye(req, tx)
	b.closeFromOutbound(context.Background())
}

func (b *inviteBridge) closeFromInbound(ctx context.Context) {
	b.close.Do(func() {
		b.unregister()
		b.outCC.Close(ctx, nil)
		b.outMedia.Close()
		b.done.Break()
	})
}

func (b *inviteBridge) closeFromOutbound(ctx context.Context) {
	b.close.Do(func() {
		b.unregister()
		b.outMedia.Close()
		b.done.Break()
		b.in.Close()
	})
}

func (b *inviteBridge) closeFromService(ctx context.Context) {
	b.closeFromInbound(ctx)
}

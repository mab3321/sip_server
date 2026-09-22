# Nayatel INVITE-based cold transfer

This fork supports cold transfer for carriers such as Nayatel that do not support
SIP `REFER`. The SIP service uses third-party call control (RFC 3725 style) and
does not require Asterisk.

## Configuration

```yaml
invite_bridge_transfer: true
invite_direct_media_transfer: true
invite_direct_media_required: true
```

`invite_bridge_transfer` enables the INVITE-based transfer path.
`invite_direct_media_transfer` attempts a direct-media handoff first.
`invite_direct_media_required` disables the RTP-anchored fallback: a failed
direct-media negotiation fails the transfer visibly instead of consuming SIP
server media resources. All options are disabled by default, so ordinary
LiveKit SIP behavior is unchanged.

The completion webhook is independently configured with
`call_completion_webhook` or the deployment's environment-backed configuration.
Do not commit webhook credentials.

## Call flow

1. The inbound caller is initially connected to the LiveKit room and agent.
2. A cold-transfer request causes this SIP service to create a second SIP dialog
   to the destination using the caller leg's SDP.
3. When the destination answers, its SDP is sent to the caller in an in-dialog
   re-INVITE.
4. On success, RTP flows directly between the two carrier legs. The LiveKit room
   and AI detach, while this service retains lightweight SIP dialog state.
5. A BYE from either party is propagated to the other dialog. Completion data is
   emitted after the complete call ends.

The SIP service does not relay or transcode RTP after a successful direct-media
handoff. When `invite_direct_media_required` is enabled, negotiation failure is
returned as a transfer error and no anchored RTP bridge is created. When it is
disabled, the service automatically falls back to the anchored INVITE bridge.

## Lifecycle safeguards

The local RTP inactivity watchdog is disabled only after direct-media handoff is
confirmed. Signaling remains active until hangup, allowing BYE propagation and
accurate completion tracking. Calls that do not use this transfer path retain
the normal media watchdog behavior.

## Deployment and rollback

The validated Nayatel image is:

```text
muhammadab3321/livekit-sip:direct-media-3bf85d9
sha256:681b2cec25537baa03247e9358fe8347c7a2bfd8950292e6c23e710520f16e0b
```

The deployment currently lives under `/opt/livekit` on the Nayatel server. Its
completion-webhook environment is `/opt/livekit/sip-call-completion.env`.

To roll back, disable the two configuration flags and deploy the previous image:

```text
muhammadab3321/livekit-sip:call-webhook-f5371a7
```

The server-side backup made before direct-media rollout is stored at
`/opt/livekit/backups/20260922-direct-media-975aedc/`.

## Validation checklist

- Confirm the destination answers and both parties have bidirectional audio.
- Confirm the logs report `invite-direct-media` establishment and do not report
  anchored fallback.
- Confirm no RTP reaches the SIP host after handoff.
- Keep the call connected beyond the historical 15-second watchdog interval.
- Hang up either side and confirm normal BYE propagation and completion webhook.
- Confirm Asterisk remains inactive.

The production validation on 2026-09-22 stayed connected for about 44 seconds,
ended with a normal Q.850 cause 16, delivered the webhook on its first attempt,
and used neither anchored fallback nor Asterisk.

package sip

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/sip/pkg/config"
)

type callCompletionPayload struct {
	Event                   string          `json:"event"`
	EventID                 string          `json:"event_id"`
	Server                  string          `json:"server"`
	LinkedID                string          `json:"linked_id"`
	CallID                  string          `json:"call_id"`
	SIPCallID               string          `json:"sip_call_id"`
	Direction               string          `json:"direction"`
	Caller                  string          `json:"caller"`
	Called                  string          `json:"called"`
	StartedAt               string          `json:"started_at"`
	AnsweredAt              string          `json:"answered_at,omitempty"`
	EndedAt                 string          `json:"ended_at"`
	ReceivedAt              string          `json:"received_at"`
	DurationSeconds         float64         `json:"duration_seconds"`
	BillableDurationSeconds float64         `json:"billable_duration_seconds"`
	Result                  string          `json:"result"`
	Reason                  string          `json:"reason"`
	HangupSource            string          `json:"hangup_source"`
	SIPStatus               int             `json:"sip_status"`
	Transfer                *transferRecord `json:"transfer,omitempty"`
	AsteriskTimezone        string          `json:"asterisk_timezone"`
	CEL                     any             `json:"cel"`
}

type transferRecord struct {
	Mode            string  `json:"mode"`
	Destination     string  `json:"destination"`
	CallID          string  `json:"call_id"`
	StartedAt       string  `json:"started_at"`
	ConnectedAt     string  `json:"connected_at"`
	DurationSeconds float64 `json:"duration_seconds"`
}

func completionTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func sendCallCompletionWebhook(log logger.Logger, conf *config.CallCompletionWebhookConfig, payload callCompletionPayload) {
	if conf == nil || conf.URL == "" {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		log.Errorw("cannot encode call completion webhook", err)
		return
	}
	client := &http.Client{Timeout: conf.Timeout}
	go func() {
		backoff := conf.RetryBackoff
		for attempt := 1; attempt <= conf.MaxAttempts; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), conf.Timeout)
			req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, conf.URL, bytes.NewReader(body))
			if reqErr == nil {
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Idempotency-Key", payload.EventID)
				if conf.BearerToken != "" {
					req.Header.Set("Authorization", "Bearer "+conf.BearerToken)
				}
				if conf.XServiceKey != "" {
					req.Header.Set("X-Service-Key", conf.XServiceKey)
				}
			}
			var status int
			if reqErr == nil {
				resp, doErr := client.Do(req)
				reqErr = doErr
				if resp != nil {
					status = resp.StatusCode
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
				if reqErr == nil && (status < 200 || status >= 300) {
					reqErr = fmt.Errorf("unexpected HTTP status %d", status)
				}
			}
			cancel()
			if reqErr == nil {
				log.Infow("call completion webhook delivered", "eventID", payload.EventID, "attempt", attempt)
				return
			}
			log.Warnw("call completion webhook delivery failed", reqErr, "eventID", payload.EventID, "attempt", attempt)
			if attempt < conf.MaxAttempts {
				time.Sleep(backoff)
				if backoff < 30*time.Second {
					backoff *= 2
				}
			}
		}
	}()
}

func completionServerName() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "livekit-sip"
	}
	return name
}

func (c *inboundCall) emitCallCompletion(end EndCall, sipStatus int) {
	conf := c.s.conf.CallCompletionWebhook
	if conf == nil || conf.URL == "" {
		return
	}
	ended := time.Now()
	callID := string(c.cc.ID())
	hangupSource := "server"
	if p := c.completionHangupSource.Load(); p != nil {
		hangupSource = *p
	}
	payload := callCompletionPayload{
		Event:                   "call.completed",
		EventID:                 completionServerName() + ":" + callID,
		Server:                  completionServerName(),
		LinkedID:                callID,
		CallID:                  callID,
		SIPCallID:               c.cc.SIPCallID(),
		Direction:               "inbound",
		Caller:                  c.cc.From().User,
		Called:                  c.cc.To().User,
		StartedAt:               completionTime(c.callStart),
		AnsweredAt:              completionTime(c.sigTs.AcceptTime),
		EndedAt:                 completionTime(ended),
		ReceivedAt:              completionTime(ended),
		DurationSeconds:         ended.Sub(c.callStart).Seconds(),
		BillableDurationSeconds: elapsedSeconds(c.sigTs.AcceptTime, ended),
		Result:                  string(end.Term.Result),
		Reason:                  end.Term.Reason,
		HangupSource:            hangupSource,
		SIPStatus:               sipStatus,
		AsteriskTimezone:        "Etc/UTC",
		CEL:                     nil,
	}
	if bridge := c.inviteBridge.Load(); bridge != nil {
		payload.Transfer = &transferRecord{
			Mode:            "invite-bridge",
			Destination:     bridge.to,
			CallID:          string(bridge.outTag),
			StartedAt:       completionTime(bridge.started),
			ConnectedAt:     completionTime(bridge.answered),
			DurationSeconds: elapsedSeconds(bridge.answered, ended),
		}
	}
	sendCallCompletionWebhook(c.log(), conf, payload)
}

func (c *outboundCall) emitCallCompletion(end EndCall) {
	conf := c.c.conf.CallCompletionWebhook
	if conf == nil || conf.URL == "" {
		return
	}
	ended := time.Now()
	callID := string(c.cc.ID())
	hangupSource := "server"
	if end.Term.Reason == "hangup" {
		hangupSource = "destination"
	} else if end.Term.Reason == "rpc" {
		hangupSource = "livekit"
	}
	payload := callCompletionPayload{
		Event:                   "call.completed",
		EventID:                 completionServerName() + ":" + callID,
		Server:                  completionServerName(),
		LinkedID:                callID,
		CallID:                  callID,
		SIPCallID:               c.cc.SIPCallID(),
		Direction:               "outbound",
		Caller:                  c.sipConf.from.Address.User,
		Called:                  c.sipConf.to.Address.User,
		StartedAt:               completionTime(c.callStart),
		AnsweredAt:              completionTime(c.sigTs.AcceptTime),
		EndedAt:                 completionTime(ended),
		ReceivedAt:              completionTime(ended),
		DurationSeconds:         ended.Sub(c.callStart).Seconds(),
		BillableDurationSeconds: elapsedSeconds(c.sigTs.AcceptTime, ended),
		Result:                  string(end.Term.Result),
		Reason:                  end.Term.Reason,
		HangupSource:            hangupSource,
		AsteriskTimezone:        "Etc/UTC",
		CEL:                     nil,
	}
	sendCallCompletionWebhook(c.log, conf, payload)
}

func elapsedSeconds(start, end time.Time) float64 {
	if start.IsZero() || end.Before(start) {
		return 0
	}
	return end.Sub(start).Seconds()
}

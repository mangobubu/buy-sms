package application

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"buysms/internal/domain"
	"buysms/internal/identity"
	"buysms/internal/provider"
)

// LocalCompleteOrder is an explicit operator recovery, not a claim that the
// provider completed an activation. It keeps the regular completion path
// strict and only permits a local close after a fresh BAD_STATUS + received.
func (s *Service) LocalCompleteOrder(ctx context.Context, id string, in LocalCompleteInput, user domain.User, ip string) (OrderDTO, error) {
	if !in.UpstreamMissingConfirmed {
		return OrderDTO{}, localCompleteNotAllowedError()
	}
	scope := ""
	if user.Role != "admin" {
		scope = user.ID
	}
	var view OrderDTO
	err := s.repo.WithOrderLock(ctx, id, func(lockCtx context.Context) error {
		o, err := s.repo.GetOrder(lockCtx, id, scope)
		if err != nil {
			return mapStore(err)
		}
		if !canLocalCompleteSMSBower(o, s.now()) {
			return localCompleteNotAllowedError()
		}
		p, key, client, err := s.providerClient(lockCtx, o.ProviderID)
		if err != nil {
			return err
		}
		confirmer, ok := client.(provider.CompletionConfirmationClient)
		if !ok {
			return localCompleteNotAllowedError()
		}
		confirmation, completeErr := confirmer.CompleteWithConfirmation(lockCtx, key, o.UpstreamID)
		s.invalidateProviderBalance(o.ProviderID)
		// Never discard a code that arrived with the confirmation. Saving failure
		// keeps the order active even if the operator acknowledged disappearance.
		if err = s.saveLocalCompletionMessages(lockCtx, o, confirmation); err != nil {
			return err
		}
		status, state := domain.OrderCompleted, "user_complete"
		local := false
		if completeErr != nil {
			if confirmedStatus, confirmedState, confirmed := confirmedSMSBowerCompletion(o, completeErr); confirmed {
				status, state = confirmedStatus, confirmedState
			} else if smsBowerLocalCompletionConflict(completeErr, confirmation) {
				state, local = "user_local_complete", true
			} else {
				logOrderCompleteFailure(o.ID, completeErr)
				return orderActionProviderError("complete", completeErr)
			}
		}
		if local {
			// A fixed reason is sufficient evidence of the explicit UI confirmation;
			// do not copy arbitrary free text or provider/SMS content into audit.
			auditMeta := json.RawMessage(`{"upstreamMissingConfirmed":true,"reason":"operator_confirmed_upstream_missing"}`)
			if err = s.repo.CompleteOrderLocally(lockCtx, o.ID, user.ID, ip, auditMeta); err != nil {
				return mapStore(err)
			}
		} else {
			if err = s.repo.SetOrderStatus(lockCtx, o.ID, status, state); err != nil {
				return mapStore(err)
			}
			auditEvent, auditMeta := orderFinishAudit("complete", "user", status, state)
			_ = s.repo.Audit(lockCtx, &user.ID, auditEvent, "order", o.ID, ip, auditMeta)
		}
		o, err = s.repo.GetOrder(lockCtx, o.ID, scope)
		if err != nil {
			return err
		}
		view = s.orderView(o, readSettings(p.Config).WebhookEnabled)
		return nil
	})
	if err != nil {
		return OrderDTO{}, mapStore(err)
	}
	return view, nil
}

func canLocalCompleteSMSBower(o domain.Order, now time.Time) bool {
	return o.ProviderID == domain.ProviderSMSBower && o.Status == domain.OrderActive &&
		!o.RenewalInflight && !o.RequestNextInflight && hasCurrentActivationMessage(o) &&
		smsBowerAutoFinishDue(o, now)
}

func smsBowerLocalCompletionConflict(err error, confirmation provider.PollResult) bool {
	var upstream *provider.ProviderError
	if !errors.As(err, &upstream) || upstream == nil || upstream.Provider != domain.ProviderSMSBower ||
		upstream.Operation != "complete" || upstream.Code != "BAD_STATUS" || upstream.Retryable ||
		upstream.ConfirmationState != provider.PollReceived || confirmation.State != provider.PollReceived {
		return false
	}
	switch upstream.HTTPStatus {
	case 0, http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

// saveLocalCompletionMessages mirrors poll identity rules, but never advances
// a resend generation or starts another provider action while closing locally.
func (s *Service) saveLocalCompletionMessages(ctx context.Context, o domain.Order, result provider.PollResult) error {
	messages := result.Messages
	if len(messages) == 0 && (result.Code != "" || result.Text != "") {
		messages = []provider.OTPMessage{{Code: result.Code, Text: result.Text}}
	}
	sequence := o.PollSequence
	for _, up := range messages {
		hasTime := !up.ReceivedAt.IsZero()
		signature := messageState(up.Code, up.Text)
		if !hasTime && signature == o.LastProviderState && strings.TrimSpace(up.Fingerprint) == "" && strings.TrimSpace(up.UpstreamID) == "" {
			continue
		}
		received := up.ReceivedAt
		if !hasTime {
			received = s.now().UTC()
		}
		fingerprint := strings.TrimSpace(up.Fingerprint)
		if fingerprint == "" {
			fingerprint = strings.TrimSpace(up.UpstreamID)
		}
		if fingerprint != "" {
			fingerprint = digestHex(o.ProviderID, o.UpstreamID, "upstream_message", fingerprint, strconv.Itoa(up.Generation))
		} else {
			fingerprint = messageFingerprintWithSequence(o, received, hasTime, up.Code, up.Text, sequence)
		}
		message := domain.SMSMessage{ID: identity.UUID(), OrderID: o.ID, ProviderID: o.ProviderID, Code: up.Code, Text: up.Text, Source: "poll", UpstreamFingerprint: fingerprint, ReceivedAt: received}
		inserted, err := s.repo.SaveMessage(ctx, message, false)
		if err != nil {
			return err
		}
		if inserted {
			sequence++
		}
	}
	return nil
}

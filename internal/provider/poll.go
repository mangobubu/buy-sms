package provider

import (
	"encoding/json"
	"strings"
	"time"
)

func parseOTPList(payload []byte) ([]OTPMessage, string, *time.Time, error) {
	value, err := decodeAny(payload)
	if err != nil {
		return nil, "", nil, err
	}
	state := ""
	var expiresAt *time.Time
	if object, ok := value.(map[string]any); ok {
		state = normalizePollState(firstScalar(object, "activationStatus", "status", "state"))
		if raw, found := lookup(object, "expiresAt", "expiration", "expiry"); found {
			expiresAt = parseTimeValue(raw)
		}
	}

	messageObjects := make([]map[string]any, 0)
	collectMessageObjects(value, &messageObjects, false)
	messages := make([]OTPMessage, 0, len(messageObjects))
	seen := make(map[string]struct{})
	for _, object := range messageObjects {
		code := firstScalar(object, "smsCode", "sms_code", "code", "otp", "value")
		text := firstScalar(object, "smsText", "sms_text", "text", "full_sms", "fullSms", "message")
		if code == "" && text == "" {
			continue
		}
		upstreamID := firstScalar(object, "id", "ID", "smsId", "sms_id", "messageId")
		var receivedAt time.Time
		if raw, found := lookup(object, "receivedAt", "received_at", "smsDate", "timestamp", "createdAt", "date"); found {
			if parsed := parseTimeValue(raw); parsed != nil {
				receivedAt = *parsed
			}
		}
		message := OTPMessage{
			UpstreamID: upstreamID,
			Code:       code,
			Text:       text,
			Type:       firstScalar(object, "type", "verificationType"),
			PhoneFrom:  firstScalar(object, "phoneFrom", "phone_from", "sender", "from"),
			ReceivedAt: receivedAt,
		}
		message.Generation, _ = firstInt(object, "generation", "resend", "resends", "sequence")
		if upstreamID != "" {
			message.Fingerprint = upstreamID
		} else {
			message.Fingerprint = fingerprint(code, text, stringValue(message.Generation), receivedAt.UTC().Format(time.RFC3339Nano))
		}
		if _, duplicate := seen[message.Fingerprint]; duplicate {
			continue
		}
		seen[message.Fingerprint] = struct{}{}
		messages = append(messages, message)
	}
	if len(messages) > 0 {
		state = PollReceived
	} else if state == "" || state == PollUnknown {
		state = PollWaiting
	}
	return messages, state, expiresAt, nil
}

func collectMessageObjects(value any, output *[]map[string]any, insideMessageList bool) {
	switch typed := value.(type) {
	case []any:
		for _, child := range typed {
			collectMessageObjects(child, output, true)
		}
	case map[string]any:
		_, hasCode := lookup(typed, "smsCode", "sms_code", "code", "otp")
		_, hasText := lookup(typed, "smsText", "sms_text", "text", "full_sms", "fullSms", "message")
		_, hasMessageIdentity := lookup(typed, "id", "ID", "smsId", "sms_id", "messageId", "receivedAt", "received_at", "smsDate", "phoneFrom")
		if (hasCode || hasText) && (insideMessageList || hasMessageIdentity) {
			*output = append(*output, typed)
			return
		}
		for key, child := range typed {
			isList := insideMessageList || strings.EqualFold(key, "data") || strings.EqualFold(key, "sms") ||
				strings.EqualFold(key, "otps") || strings.EqualFold(key, "otpList") ||
				strings.EqualFold(key, "messages") || strings.EqualFold(key, "items")
			collectMessageObjects(child, output, isList)
		}
	}
}

func normalizePollState(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "unknown":
		return PollUnknown
	case "1", "pending", "waiting", "wait", "status_wait_code":
		return PollWaiting
	case "4", "resend", "wait_retry", "status_wait_retry":
		return PollWaitingRetry
	case "7", "8", "processing", "activating", "reserved":
		return PollProcessing
	case "3", "received", "code_received", "status_ok":
		return PollReceived
	case "complete", "completed", "finished", "finish":
		return PollCompleted
	case "5", "cancel", "canceled", "cancelled", "status_cancel":
		return PollCanceled
	case "2", "expired", "timeout":
		return PollExpired
	case "6", "refunded", "refund":
		return PollRefunded
	default:
		return PollUnknown
	}
}

func applyPollConvenience(result *PollResult) {
	if len(result.Messages) == 0 {
		return
	}
	last := result.Messages[len(result.Messages)-1]
	result.Code = last.Code
	result.Text = last.Text
}

func singleOTP(code, text string, receivedAt time.Time) OTPMessage {
	return OTPMessage{
		Code: code, Text: text, ReceivedAt: receivedAt,
		Fingerprint: fingerprint(code, text, receivedAt.UTC().Format(time.RFC3339Nano)),
	}
}

func cloneRaw(payload []byte) json.RawMessage {
	return append(json.RawMessage(nil), payload...)
}

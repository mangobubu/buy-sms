package provider

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"buysms/internal/domain"
)

func (c *SMSBower) Poll(ctx context.Context, apiKey, upstreamID string) (PollResult, error) {
	result, err := c.smsActivateClient.Poll(ctx, apiKey, upstreamID)
	if !smsBowerNoActivation(err) {
		return result, err
	}
	// getAllSms 的 NO_ACTIVATION 可能只是兼容接口未开放。只以标准
	// getStatus 的再次确认作为订单消失的证据，不把普通 HTTP 404 当终态。
	return c.activationStatus(ctx, apiKey, upstreamID, "poll.confirm")
}

func (c *SMSBower) Complete(ctx context.Context, apiKey, upstreamID string) error {
	_, err := c.completeWithConfirmation(ctx, apiKey, upstreamID, false)
	return err
}

// CompleteWithConfirmation performs setStatus(6) and, for an unconfirmed
// BAD_STATUS/NO_ACTIVATION response, returns the read-only getStatus result.
// The result is kept out of ProviderError so callers can persist any code
// before deciding whether a local recovery is allowed.
func (c *SMSBower) CompleteWithConfirmation(ctx context.Context, apiKey, upstreamID string) (PollResult, error) {
	return c.completeWithConfirmation(ctx, apiKey, upstreamID, true)
}

func (c *SMSBower) completeWithConfirmation(ctx context.Context, apiKey, upstreamID string, preserveMessages bool) (PollResult, error) {
	err := c.smsActivateClient.Complete(ctx, apiKey, upstreamID)
	if !smsBowerNoActivation(err) && !smsBowerBadStatus(err) {
		return PollResult{}, err
	}
	// BAD_STATUS 也可能来自已结束的订单。只读核对实际状态，不重复提交
	// setStatus，也不把状态拒绝、超时或仍在接码当作远端完成成功。
	result, confirmErr := c.activationStatusWithMessages(ctx, apiKey, upstreamID, "complete.confirm", preserveMessages)
	if confirmErr != nil {
		return PollResult{}, confirmErr
	}
	switch result.State {
	case PollMissing:
		// 由应用层结合本地短信决定是否可完成；这里不声称远端完成成功。
		return result, &ProviderError{Provider: domain.ProviderSMSBower, Operation: "complete",
			Code: CodeActivationMissing, ConfirmationState: PollMissing}
	case PollCompleted, PollCanceled, PollExpired, PollRefunded:
		return result, &ProviderError{Provider: domain.ProviderSMSBower, Operation: "complete",
			Code: CodeActivationTerminal, ConfirmationState: result.State}
	default:
		var upstream *ProviderError
		if errors.As(err, &upstream) && upstream != nil {
			confirmed := *upstream
			confirmed.ConfirmationState = result.State
			return result, &confirmed
		}
	}
	return result, err
}

func (c *SMSBower) activationStatus(ctx context.Context, apiKey, upstreamID, operation string) (PollResult, error) {
	return c.activationStatusWithMessages(ctx, apiKey, upstreamID, operation, false)
}

func (c *SMSBower) activationStatusWithMessages(ctx context.Context, apiKey, upstreamID, operation string, preserveMessages bool) (PollResult, error) {
	query := url.Values{"action": {"getStatus"}, "id": {upstreamID}}
	payload, err := c.http.get(ctx, operation, apiKey, "", query, false)
	if err == nil {
		err = c.businessError(operation, apiKey, payload)
	}
	if smsBowerNoActivation(err) {
		return PollResult{State: PollMissing}, nil
	}
	if err != nil {
		return PollResult{}, err
	}
	result, parseErr := c.parsePoll(payload)
	var upstream *ProviderError
	if errors.As(parseErr, &upstream) && upstream != nil {
		failure := *upstream
		failure.Operation = operation
		return PollResult{}, &failure
	}
	if isTerminalPollState(result.State) && !smsBowerExplicitTerminal(payload, result.State) {
		// 兼容层的数字状态码映射来自其他供应商，不能用来确认 SMSBower
		// 的结束/退款；只接受标准取消文本或明确的文字终态。
		return PollResult{}, c.http.failure(operation, "INVALID_RESPONSE", 0, false, nil)
	}
	if !preserveMessages && operation == "complete.confirm" && isTerminalPollState(result.State) && len(result.Messages) > 0 {
		// Complete 的错误契约只传递状态，不能携带短信。确认响应还含短信
		// 时不提前关闭订单，留给正常轮询保存，避免遗漏最后一条验证码。
		return PollResult{}, c.http.failure(operation, "INVALID_RESPONSE", 0, false, nil)
	}
	return result, parseErr
}

func smsBowerExplicitTerminal(payload []byte, state string) bool {
	if strings.EqualFold(strings.TrimSpace(string(payload)), "STATUS_CANCEL") {
		return state == PollCanceled
	}
	value, err := decodeAny(payload)
	object, ok := value.(map[string]any)
	if err != nil || !ok {
		return false
	}
	status := strings.ToLower(firstScalar(object, "activationStatus", "status", "state"))
	switch status {
	case "complete", "completed", "finished", "finish":
		return state == PollCompleted
	case "cancel", "canceled", "cancelled", "status_cancel":
		return state == PollCanceled
	case "expired", "timeout":
		return state == PollExpired
	case "refunded", "refund":
		return state == PollRefunded
	default:
		return false
	}
}

func smsBowerBadStatus(err error) bool {
	var upstream *ProviderError
	if !errors.As(err, &upstream) || upstream == nil || upstream.Provider != domain.ProviderSMSBower ||
		upstream.Code != "BAD_STATUS" || upstream.Retryable {
		return false
	}
	switch upstream.HTTPStatus {
	case 0, http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

func smsBowerNoActivation(err error) bool {
	var upstream *ProviderError
	if !errors.As(err, &upstream) || upstream == nil || upstream.Provider != domain.ProviderSMSBower ||
		upstream.Code != "NO_ACTIVATION" || upstream.Retryable {
		return false
	}
	// 认证错误、限流和服务器故障即使携带同名错误码，也不是有效确认。
	switch upstream.HTTPStatus {
	case 0, http.StatusBadRequest, http.StatusNotFound:
		return true
	default:
		return false
	}
}

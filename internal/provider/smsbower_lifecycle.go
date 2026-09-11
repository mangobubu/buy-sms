package provider

import (
	"context"
	"errors"
	"net/http"
	"net/url"

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
	err := c.smsActivateClient.Complete(ctx, apiKey, upstreamID)
	if !smsBowerNoActivation(err) {
		return err
	}
	result, confirmErr := c.activationStatus(ctx, apiKey, upstreamID, "complete.confirm")
	if confirmErr != nil {
		return confirmErr
	}
	if result.State == PollMissing {
		// 由应用层结合本地短信决定是否可完成；这里不声称远端完成成功。
		return c.http.failure("complete", CodeActivationMissing, 0, false, nil)
	}
	return err
}

func (c *SMSBower) activationStatus(ctx context.Context, apiKey, upstreamID, operation string) (PollResult, error) {
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
	return c.parsePoll(payload)
}

func smsBowerNoActivation(err error) bool {
	var upstream *ProviderError
	if !errors.As(err, &upstream) || upstream.Provider != domain.ProviderSMSBower ||
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

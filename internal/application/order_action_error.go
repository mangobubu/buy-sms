package application

import (
	"errors"
	"strings"

	"buysms/internal/provider"
)

const (
	OrderActionCodeCancelNotAvailableYet      = "cancel_not_available_yet"
	OrderActionCodeCancelNotAllowed           = "cancel_not_allowed"
	OrderActionCodeRenewalIdempotencyMismatch = "renewal_idempotency_mismatch"
	OrderActionCodeRenewalNotAvailable        = "renewal_not_available"
	OrderActionCodeRenewalPriceChanged        = "renewal_price_changed"
	OrderActionCodeRenewalInProgress          = "renewal_in_progress"
	OrderActionCodeRenewalResultUnknown       = "renewal_result_unknown"
	OrderActionCodeRenewalNoBalance           = "renewal_insufficient_balance"
	OrderActionCodeProviderError              = "provider_error"
)

// OrderActionError 是完成/取消订单接口的稳定错误契约。Kind 保留 errors.Is
// 兼容，Code 供 HTTP 客户端判断，Cause 仅用于服务端错误链且不会写入响应。
type OrderActionError struct {
	Action  string
	Code    string
	Message string
	Kind    error
	Cause   error
}

func (e *OrderActionError) Error() string { return e.Message }

func (e *OrderActionError) Unwrap() []error {
	errs := make([]error, 0, 2)
	if e.Kind != nil {
		errs = append(errs, e.Kind)
	}
	if e.Cause != nil && !errors.Is(e.Cause, e.Kind) {
		errs = append(errs, e.Cause)
	}
	return errs
}

func cancelPolicyError(decision CancelDecision) *OrderActionError {
	message := decision.UnavailableReason
	if message == "" {
		message = "当前订单不能取消"
	}
	if decision.ErrorCode == OrderActionCodeCancelNotAvailableYet {
		message += "，请稍后重试"
	}
	return &OrderActionError{
		Action:  "cancel",
		Code:    decision.ErrorCode,
		Message: message,
		Kind:    ErrConflict,
	}
}

func orderActionProviderError(action string, cause error) *OrderActionError {
	if action == "cancel" && providerCancelNotAvailableYet(cause) {
		return &OrderActionError{
			Action:  action,
			Code:    OrderActionCodeCancelNotAvailableYet,
			Message: "号码暂时还不能取消，请稍后重试",
			Kind:    ErrConflict,
			Cause:   cause,
		}
	}
	if action == "cancel" {
		if message, denied := providerCancelNotAllowed(cause); denied {
			return &OrderActionError{
				Action:  action,
				Code:    OrderActionCodeCancelNotAllowed,
				Message: message,
				Kind:    ErrConflict,
				Cause:   cause,
			}
		}
	}
	return &OrderActionError{
		Action:  action,
		Code:    OrderActionCodeProviderError,
		Message: "供应商暂时不可用，请稍后重试",
		Kind:    ErrProvider,
		Cause:   cause,
	}
}

func providerCancelNotAllowed(err error) (string, bool) {
	var upstream *provider.ProviderError
	if !errors.As(err, &upstream) {
		return "", false
	}
	switch strings.ToUpper(strings.TrimSpace(upstream.Code)) {
	case "FREE_CANCELLATION_EXPIRED":
		return "长租号码的免费取消期限已过，当前号码不能取消", true
	case "OTP_RECEIVED", "NEW_OTP_RECEIVED":
		return "号码已收到短信，不能取消", true
	case "ACTIVATION_NOT_ACTIVE":
		return "号码已结束，不能取消", true
	default:
		return "", false
	}
}

func providerCancelNotAvailableYet(err error) bool {
	var upstream *provider.ProviderError
	if !errors.As(err, &upstream) {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(upstream.Code)) {
	case provider.CodeCancelNotAvailableYet, "EARLY_CANCEL_DENIED":
		return true
	default:
		return false
	}
}
func renewalIdempotencyMismatchError() *OrderActionError {
	return &OrderActionError{
		Action: "renew", Code: OrderActionCodeRenewalIdempotencyMismatch,
		Message: "该续期请求编号已用于其他订单或选项，请刷新后重新确认", Kind: ErrConflict,
	}
}
func renewalNotAvailableError() *OrderActionError {
	return &OrderActionError{
		Action: "renew", Code: OrderActionCodeRenewalNotAvailable,
		Message: "该号码当前没有可用的续期方案", Kind: ErrConflict,
	}
}

func renewalPriceChangedError() *OrderActionError {
	return &OrderActionError{
		Action: "renew", Code: OrderActionCodeRenewalPriceChanged,
		Message: "供应商续期价格已变化，请重新确认最新报价", Kind: ErrConflict,
	}
}

func renewalInProgressError() *OrderActionError {
	return &OrderActionError{
		Action: "renew", Code: OrderActionCodeRenewalInProgress,
		Message: "该号码的续期请求正在处理或结果待确认，请先核对供应商订单", Kind: ErrConflict,
	}
}

func renewalResultUnknownError(cause error) *OrderActionError {
	return &OrderActionError{
		Action: "renew", Code: OrderActionCodeRenewalResultUnknown,
		Message: "续期结果尚未确认：供应商可能已经扣费，为避免重复扣费，已暂停该号码再次续期，请先核对供应商订单",
		Kind:    ErrProvider, Cause: cause,
	}
}

func renewalProviderError(cause error, submitted bool) *OrderActionError {
	if providerRenewalUnavailable(cause) {
		err := renewalNotAvailableError()
		err.Cause = cause
		return err
	}
	code := providerRenewalCode(cause)
	switch code {
	case "NO_BALANCE", "INSUFFICIENT_BALANCE", "INSUFFICIENTBALANCE", "INSUFFICIENT_FUNDS", "INSUFFICIENTFUNDS", "NOT_ENOUGH_BALANCE", "NOTENOUGHBALANCE", "LOW_BALANCE", "LOWBALANCE":
		return &OrderActionError{
			Action: "renew", Code: OrderActionCodeRenewalNoBalance,
			Message: "供应商账户余额不足，续期尚未提交", Kind: ErrProvider, Cause: cause,
		}
	}
	if submitted && providerRenewalOutcomeUnknown(cause) {
		return renewalResultUnknownError(cause)
	}
	return &OrderActionError{
		Action: "renew", Code: OrderActionCodeProviderError,
		Message: "获取续期信息失败，请稍后重试", Kind: ErrProvider, Cause: cause,
	}
}

func providerRenewalUnavailable(err error) bool {
	var upstream *provider.ProviderError
	if !errors.As(err, &upstream) {
		return false
	}
	if upstream.HTTPStatus == 404 || upstream.HTTPStatus == 425 {
		return true
	}
	switch providerRenewalCode(err) {
	case "TOO_EARLY", "FREEZE_PERIOD_NOT_REACHED", "ACTION_NOT_AVAILABLE",
		"SIM_OFFLINE", "SIM_TEMPORARY_OFFLINE", "SERVICE_NOT_AVAILABLE",
		"ACTIVATION_NOT_FOUND", "ORDER_NOT_FOUND", "NOT_FOUND", "NOTFOUND",
		"RENEW_ACTIVATION_NOT_AVAILABLE", "NEW_ACTIVATION_IMPOSSIBLE", "UPSTREAM_REJECTED":
		return true
	default:
		return false
	}
}

func providerRenewalOutcomeUnknown(err error) bool {
	var upstream *provider.ProviderError
	if !errors.As(err, &upstream) {
		return true
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(upstream.Operation)), "renewal.result.") ||
		upstream.HTTPStatus >= 500 || upstream.HTTPStatus == 429 {
		return true
	}
	switch providerRenewalCode(err) {
	case "TIMEOUT", "TRANSPORT_ERROR", "READ_ERROR", "NO_CONNECTION", "CANCELED",
		"RESPONSE_TOO_LARGE", "INVALID_RESPONSE", "RESULT_NOT_CONFIRMED":
		return true
	default:
		return false
	}
}

func providerRenewalCode(err error) string {
	var upstream *provider.ProviderError
	if !errors.As(err, &upstream) {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(upstream.Code))
}

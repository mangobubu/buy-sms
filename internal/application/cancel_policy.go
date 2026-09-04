package application

import (
	"strconv"
	"strings"
	"time"

	"buysms/internal/domain"
)

const (
	timedCancelDelay             = 2 * time.Minute
	heroRentalCancelRefundWindow = 20 * time.Minute
)

const (
	cancelReasonOrderFinished = "订单已结束，不能取消"
	cancelReasonOrderExpired  = "号码已过期，不能取消"
	cancelReasonMessageExists = "号码已收到短信，不能取消"
	cancelReasonWaiting       = "号码购买满2分钟后才可取消"
	cancelReasonRefundExpired = "长租号码购买满20分钟后不能取消"
	cancelReasonUnsupported   = "当前供应商不支持取消"
)

// CancelDecision 是订单取消能力的唯一事实源。接口展示和真正执行取消都应
// 使用同一份评估结果，避免页面显示可取消但服务端采用另一套规则。
type CancelDecision struct {
	Allowed           bool
	AvailableAt       *time.Time
	WaitSeconds       int
	UnavailableReason string
	ErrorCode         string
}

// EvaluateCancelPolicy 只依赖订单快照与显式传入的时间，可在不连接数据库或
// 供应商的情况下独立测试。SMSPool 的等待时长没有公开的固定值，因此这里
// 不设置本地倒计时，具体的短暂锁定由供应商响应决定。
func EvaluateCancelPolicy(order domain.Order, now time.Time) CancelDecision {
	if order.RenewalInflight {
		return CancelDecision{
			UnavailableReason: "号码续期结果确认中，暂不可取消",
			ErrorCode:         OrderActionCodeRenewalInProgress,
		}
	}
	if order.Status != domain.OrderActive {
		return CancelDecision{
			UnavailableReason: cancelReasonOrderFinished,
			ErrorCode:         "cancel_not_allowed",
		}
	}
	if order.ExpiresAt != nil && !order.ExpiresAt.After(now) {
		return CancelDecision{
			UnavailableReason: cancelReasonOrderExpired,
			ErrorCode:         "cancel_not_allowed",
		}
	}
	if order.NonRefundable {
		return CancelDecision{
			UnavailableReason: "该号码重新启用后不支持退款取消",
			ErrorCode:         OrderActionCodeCancelNotAllowed,
		}
	}
	if hasCurrentActivationMessage(order) {
		return CancelDecision{
			UnavailableReason: cancelReasonMessageExists,
			ErrorCode:         "cancel_not_allowed",
		}
	}

	switch domain.NormalizeProvider(order.ProviderID) {
	case domain.ProviderHeroSMS:
		purchasedAt := order.ActivationStartedAt
		if purchasedAt.IsZero() {
			purchasedAt = order.CreatedAt
		}
		rentalHours, longRental := heroLongRentalHours(order.Duration)
		if longRental && order.ExpiresAt != nil &&
			rentalHours <= uint64((1<<63-1)/int64(time.Hour)) {
			upstreamStartedAt := order.ExpiresAt.Add(-time.Duration(rentalHours) * time.Hour)
			if purchasedAt.IsZero() || upstreamStartedAt.Before(purchasedAt) {
				purchasedAt = upstreamStartedAt
			}
		}
		if purchasedAt.IsZero() {
			return CancelDecision{Allowed: true}
		}
		if longRental && !now.Before(purchasedAt.Add(heroRentalCancelRefundWindow)) {
			return CancelDecision{
				UnavailableReason: cancelReasonRefundExpired,
				ErrorCode:         OrderActionCodeCancelNotAllowed,
			}
		}
		availableAt := purchasedAt.Add(timedCancelDelay)
		decision := CancelDecision{AvailableAt: &availableAt}
		if now.Before(availableAt) {
			remaining := availableAt.Sub(now)
			decision.WaitSeconds = int((remaining-1)/time.Second) + 1
			decision.UnavailableReason = cancelReasonWaiting
			decision.ErrorCode = "cancel_not_available_yet"
			return decision
		}
		decision.Allowed = true
		return decision
	case domain.ProviderSMSBower, domain.ProviderSMSPool:
		return CancelDecision{Allowed: true}
	default:
		return CancelDecision{
			UnavailableReason: cancelReasonUnsupported,
			ErrorCode:         "cancel_not_allowed",
		}
	}
}

func hasCurrentActivationMessage(order domain.Order) bool {
	if len(order.Messages) == 0 {
		return false
	}
	if order.ActivationStartedAt.IsZero() {
		return true
	}
	for _, message := range order.Messages {
		if !message.ReceivedAt.Before(order.ActivationStartedAt) {
			return true
		}
	}
	return false
}
func heroLongRentalHours(duration string) (uint64, bool) {
	hours, err := strconv.ParseUint(strings.TrimSpace(duration), 10, 31)
	return hours, err == nil && hours >= 24
}

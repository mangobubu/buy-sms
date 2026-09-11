package application

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"buysms/internal/domain"
	"buysms/internal/identity"
	"buysms/internal/provider"
	"buysms/internal/store"
)

const smsBowerAutoFinishDuration = 25 * time.Minute

// SMSbower 的 25 分钟是本站的自动处理规则，不是供应商返回的号码期限。
// 以持久化的创建时间计算，因此页面关闭或服务重启不会重置倒计时。
func smsBowerAutoFinishAt(order domain.Order) (time.Time, bool) {
	if order.ProviderID != domain.ProviderSMSBower || order.CreatedAt.IsZero() {
		return time.Time{}, false
	}
	return order.CreatedAt.Add(smsBowerAutoFinishDuration), true
}

func smsBowerAutoFinishDue(order domain.Order, now time.Time) bool {
	deadline, ok := smsBowerAutoFinishAt(order)
	return ok && !now.Before(deadline)
}

func nextSuccessfulPollAt(order domain.Order, candidate time.Time) time.Time {
	next := nextPollAt(candidate, order.ExpiresAt)
	if deadline, ok := smsBowerAutoFinishAt(order); ok && deadline.Before(next) {
		return deadline
	}
	return next
}

// 到期时锁可能正由跨越截止时间的续码或人工动作持有。只缩短数据库租约，
// 不覆盖持锁操作的状态、更新时间、短信指纹或失败次数；终态行保持不动。
func (s *Service) rescheduleSMSBowerDeadlinePoll(ctx context.Context, order domain.Order) {
	if !smsBowerAutoFinishDue(order, s.now()) {
		return
	}
	if err := s.repo.RescheduleClaimedOrderPoll(ctx, order.ID, order.UpstreamID, order.NextPollAt, s.now().Add(5*time.Second)); err != nil {
		slog.Warn("SMSbower 到期轮询重排失败", "order_id", order.ID, "error", err)
	}
}

func smsBowerDeadlineSnapshotChanged(snapshot, fresh domain.Order) bool {
	return snapshot.ProviderID == domain.ProviderSMSBower && fresh.ProviderID == snapshot.ProviderID &&
		fresh.Status == domain.OrderActive && !fresh.RenewalInflight && fresh.UpstreamID == snapshot.UpstreamID
}

// 调用者必须持有订单锁，并已成功保存本轮消息、处理供应商终态。
// handled 同时覆盖成功和重试，阻止本轮再次续码或覆盖失败退避时间。
func (s *Service) autoFinishSMSBowerLocked(ctx context.Context, order domain.Order, key string, client provider.Client, result provider.PollResult, state string) (handled bool, err error) {
	if !smsBowerAutoFinishDue(order, s.now()) {
		return false, nil
	}
	fresh, err := s.repo.GetOrder(ctx, order.ID, "")
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		s.pollFailure(ctx, order, state)
		return true, err
	}
	if fresh.Status != domain.OrderActive || fresh.RenewalInflight ||
		fresh.ProviderID != order.ProviderID || fresh.UpstreamID != order.UpstreamID {
		return true, nil
	}
	if !smsBowerAutoFinishDue(fresh, s.now()) {
		return false, nil
	}

	// STATUS_WAIT_RETRY 的 lastCode 是已经收到过的短信，不是新的短信代次。
	// 仅在历史缺失时补存证据；完成失败后即便下一轮退回 WAIT_CODE，也不能取消。
	lastCode := strings.TrimSpace(result.LastCode)
	if len(fresh.Messages) == 0 && result.State == provider.PollWaitingRetry && lastCode != "" {
		message := domain.SMSMessage{
			ID: identity.UUID(), OrderID: fresh.ID, ProviderID: fresh.ProviderID,
			Code: lastCode, Source: "poll", ReceivedAt: s.now().UTC(),
			UpstreamFingerprint: digestHex(fresh.ProviderID, fresh.UpstreamID, "auto_finish_last_code", lastCode),
		}
		if _, err = s.repo.SaveMessage(ctx, message, false); err != nil {
			s.pollFailure(ctx, fresh, state)
			return true, err
		}
		fresh.Messages = append(fresh.Messages, message)
	}

	action, status := "cancel", domain.OrderCanceled
	if len(fresh.Messages) > 0 {
		action, status = "complete", domain.OrderCompleted
		err = completeProviderOrder(ctx, client, key, fresh.UpstreamID, fresh.Duration)
	} else {
		err = cancelProviderOrder(ctx, client, key, fresh.UpstreamID, fresh.Duration)
	}
	s.invalidateProviderBalance(fresh.ProviderID)
	providerState := "auto_" + action
	if action == "complete" {
		if confirmedStatus, confirmedState, confirmed := confirmedSMSBowerCompletion(fresh, err); confirmed {
			// 完成接口确认的真实终态与人工处理共用严格校验，不伪造完成。
			status, providerState, err = confirmedStatus, confirmedState, nil
		}
	}
	if err != nil {
		// 保留消息指纹用于下一次轮询去重；失败次数负责退避，不把业务截止
		// 时间写入 ExpiresAt，也不把未确认的远端动作当作本地成功。
		s.pollFailure(ctx, fresh, state)
		fields := append(orderActionFailureFields(fresh.ID, err), "action", action)
		slog.Warn("SMSbower 倒计时结束处理失败，将重试", fields...)
		return true, nil
	}
	if err = s.repo.SetOrderStatus(ctx, fresh.ID, status, providerState); err != nil {
		s.pollFailure(ctx, fresh, state)
		return true, err
	}
	auditEvent, auditMeta := orderFinishAudit(action, "auto", status, providerState)
	_ = s.repo.Audit(ctx, nil, auditEvent, "order", fresh.ID, "", auditMeta)
	return true, nil
}

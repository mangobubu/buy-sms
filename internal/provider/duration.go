package provider

import (
	"context"
	"strconv"
	"strings"
)

// DurationOption 描述供应商当前可购买的普通激活或长租时长。Value 为空表示
// 普通激活；长租时 Value 是规范化后的正整数小时字符串。
type DurationOption struct {
	Value     string
	Minutes   int
	Hours     int
	Price     float64
	Available int
}

// DurationCatalogClient 是可选能力，避免要求不支持租期的供应商实现空方法。
type DurationCatalogClient interface {
	Durations(context.Context, string, CatalogRequest) ([]DurationOption, error)
}

// RentalDurationCatalogClient 只读取长租报价，供购买前重新校验使用，避免为
// 每次长租购买额外查询普通激活寿命和普通报价。
type RentalDurationCatalogClient interface {
	RentalDurations(context.Context, string, CatalogRequest) ([]DurationOption, error)
}

// DurationLifecycleClient 固定处理带时长订单的后续动作。HeroSMS 在使用
// 兼容地址配置时，长租仍通过原生接口购买，因此轮询、完成、取消和续码也
// 必须保持同一原生协议通道。
type DurationLifecycleClient interface {
	PollDuration(context.Context, string, string) (PollResult, error)
	CompleteDuration(context.Context, string, string) error
	CancelDuration(context.Context, string, string) error
	RequestAnotherDuration(context.Context, string, string) (RequestAnotherResult, error)
}

func normalizeRentalDuration(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	hours, err := strconv.ParseUint(value, 10, 31)
	if err != nil || hours == 0 || strconv.FormatUint(hours, 10) != value {
		return "", ErrInvalidRequest
	}
	return value, nil
}

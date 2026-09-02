package provider

import "buysms/internal/domain"

const defaultSMSBowerBaseURL = "https://smsbower.page/stubs/handler_api.php"

type SMSBower struct {
	*smsActivateClient
}

func NewSMSBower(baseURL string, options ...Option) *SMSBower {
	if baseURL == "" {
		baseURL = defaultSMSBowerBaseURL
	}
	config := resolveOptions(options...)
	return &SMSBower{smsActivateClient: newSMSActivateClient(domain.ProviderSMSBower, baseURL, config)}
}

// NewSMSBrower 兼容用户侧常见的历史拼写。
func NewSMSBrower(baseURL string, options ...Option) *SMSBower {
	return NewSMSBower(baseURL, options...)
}

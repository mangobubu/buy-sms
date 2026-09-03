package provider

import "testing"

func TestSafeCodeFromBodySupportsProblemTitle(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "HeroSMS错误标题优先于HTTP状态",
			payload: `{"title":"WRONG_MAX_PRICE","status":422}`,
			want:    "WRONG_MAX_PRICE",
		},
		{
			name:    "大小写不敏感",
			payload: `{"Title":"NO_NUMBERS","status":422}`,
			want:    "NO_NUMBERS",
		},
		{
			name:    "传统错误码仍优先",
			payload: `{"code":"RATE_LIMIT","title":"Too Many Requests","status":429}`,
			want:    "RATE_LIMIT",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := safeCodeFromBody([]byte(test.payload)); got != test.want {
				t.Fatalf("错误码=%q，期望 %q", got, test.want)
			}
		})
	}
}

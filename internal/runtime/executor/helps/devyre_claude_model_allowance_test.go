package helps

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

// zephyrusFableRejection is the shape of the 429s that held six accounts' every model on
// 2026-11-16: the router's log named only Fable's window and overage as rejected and the claim
// seven_day_overage_included, and the shared windows' room was not confirmed by the response.
func zephyrusFableRejection() http.Header {
	reset := time.Now().Add(137 * time.Hour).Unix()
	return http.Header{
		"Anthropic-Ratelimit-Unified-Status":               []string{"rejected"},
		"Anthropic-Ratelimit-Unified-Representative-Claim": []string{"seven_day_overage_included"},
		"Anthropic-Ratelimit-Unified-Reset":                []string{strconv.FormatInt(reset, 10)},
		"Anthropic-Ratelimit-Unified-7d_oi-Status":         []string{"rejected"},
		"Anthropic-Ratelimit-Unified-7d_oi-Reset":          []string{strconv.FormatInt(reset, 10)},
		"Anthropic-Ratelimit-Unified-Overage-Status":       []string{"rejected"},
	}
}

func TestDevyreModelAllowanceRejectionHoldsTheModelAlone(t *testing.T) {
	tests := []struct {
		name      string
		headers   func() http.Header
		wantWhole bool
	}{
		{
			name:      "Fable's allowance, the shared windows not reported",
			headers:   zephyrusFableRejection,
			wantWhole: false,
		},
		{
			name: "Opus's allowance, the shared windows not reported",
			headers: func() http.Header {
				return http.Header{
					"Anthropic-Ratelimit-Unified-Status":               []string{"rejected"},
					"Anthropic-Ratelimit-Unified-Representative-Claim": []string{"seven_day_opus"},
				}
			},
			wantWhole: false,
		},
		{
			name: "Sonnet's allowance, the weekly window allowed and the 5-hour one at half",
			headers: func() http.Header {
				return http.Header{
					"Anthropic-Ratelimit-Unified-Status":               []string{"rejected"},
					"Anthropic-Ratelimit-Unified-Representative-Claim": []string{"Seven_Day_Sonnet"},
					"Anthropic-Ratelimit-Unified-7d-Status":            []string{"allowed_warning"},
					"Anthropic-Ratelimit-Unified-5h-Utilization":       []string{"0.5"},
				}
			},
			wantWhole: false,
		},
		{
			name: "Fable's allowance, the 5-hour window reported full by its utilization",
			headers: func() http.Header {
				h := zephyrusFableRejection()
				h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "1.0")
				return h
			},
			wantWhole: true,
		},
		{
			name: "Fable's allowance, the weekly window rejected",
			headers: func() http.Header {
				h := zephyrusFableRejection()
				h.Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
				return h
			},
			wantWhole: true,
		},
		{
			name: "Fable's allowance, the weekly window's utilization unreadable",
			headers: func() http.Header {
				h := zephyrusFableRejection()
				h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "NaN")
				return h
			},
			wantWhole: true,
		},
		{
			name: "Fable's allowance, a 5-hour status this code does not know",
			headers: func() http.Header {
				h := zephyrusFableRejection()
				h.Set("Anthropic-Ratelimit-Unified-5h-Status", "throttled")
				return h
			},
			wantWhole: true,
		},
		{
			name: "the weekly limit every model shares, its status not reported",
			headers: func() http.Header {
				return http.Header{
					"Anthropic-Ratelimit-Unified-Status":               []string{"rejected"},
					"Anthropic-Ratelimit-Unified-Representative-Claim": []string{"seven_day"},
				}
			},
			wantWhole: true,
		},
		{
			name: "the 5-hour limit every model shares, with overage refused",
			headers: func() http.Header {
				return http.Header{
					"Anthropic-Ratelimit-Unified-Status":               []string{"rejected"},
					"Anthropic-Ratelimit-Unified-Representative-Claim": []string{"five_hour"},
					"Anthropic-Ratelimit-Unified-5h-Status":            []string{"rejected"},
					"Anthropic-Ratelimit-Unified-Overage-Status":       []string{"rejected"},
				}
			},
			wantWhole: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClaudeHeadersIndicateUnifiedRateLimitRejection(tt.headers()); got != tt.wantWhole {
				t.Fatalf("ClaudeHeadersIndicateUnifiedRateLimitRejection() = %v, want %v", got, tt.wantWhole)
			}
		})
	}
}

// A rejection on the model's own allowance is no deadline for the credential: the model backs off
// on its own, and the reading of its window keeps it from the credential until that window resets.
func TestDevyreModelAllowanceRejectionNamesNoCredentialDeadline(t *testing.T) {
	headers := zephyrusFableRejection()
	headers.Set("Retry-After", "493200")
	if got := ParseClaudeRateLimitReset(headers, time.Now()); got != nil {
		t.Fatalf("ParseClaudeRateLimitReset() = %v, want nil for a rejection on Fable's own allowance", *got)
	}
}

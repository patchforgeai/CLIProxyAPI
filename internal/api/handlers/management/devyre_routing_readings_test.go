package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotareading"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/routingfocus"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// readingsTestNow is the fixed clock of the quota-readings tests.
var readingsTestNow = time.Date(2026, 10, 3, 21, 0, 0, 0, time.UTC)

// readingsClaudeSignals renders anthropic-ratelimit-unified-* headers the way
// they are captured in Auth.Quota.Signals: utilization as a fraction, reset as
// unix seconds.
func readingsClaudeSignals(windows map[string][2]float64) map[string]string {
	signals := make(map[string]string)
	for name, window := range windows {
		utilization, resetsInHours := window[0], window[1]
		prefix := "Anthropic-Ratelimit-Unified-" + name + "-"
		signals[prefix+"Utilization"] = strconv.FormatFloat(utilization, 'f', -1, 64)
		signals[prefix+"Reset"] = strconv.FormatInt(readingsTestNow.Add(time.Duration(resetsInHours*float64(time.Hour))).Unix(), 10)
	}
	return signals
}

func readingsFileAuth(id, provider, index string) *coreauth.Auth {
	return &coreauth.Auth{ID: id, FileName: id, Provider: provider, Index: index, Status: coreauth.StatusActive}
}

func readingsHeaderAuth(id, index string, observedAgo time.Duration, windows map[string][2]float64) *coreauth.Auth {
	auth := readingsFileAuth(id, "claude", index)
	auth.Quota = coreauth.QuotaState{ObservedAt: readingsTestNow.Add(-observedAgo), Signals: readingsClaudeSignals(windows)}
	return auth
}

// readingsGoldenFixture is a pool with every ranking case: header and stored
// readings, a gate, an exhausted weekly window, a model-scoped window, a passed
// reset, missing data, low headroom in a short and in a long window, a lower
// priority tier, a cooling credential, a disabled one and an API key.
func readingsGoldenFixture() ([]*coreauth.Auth, *quotareading.Store) {
	now := readingsTestNow
	store := quotareading.NewStore()
	window := func(id string, kind quotareading.Kind, model string, used float64, resetsIn, length, observedAgo time.Duration, source quotareading.Source) quotareading.Window {
		return quotareading.Window{ID: id, Kind: kind, Model: model, UsedPercent: used, ResetsAt: now.Add(resetsIn), Length: length, ObservedAt: now.Add(-observedAgo), Source: source}
	}
	const week = 7 * 24 * time.Hour

	alpha := readingsHeaderAuth("claude-alpha.json", "idx-alpha", 5*time.Minute, map[string][2]float64{"5h": {0.3, 2}, "7d": {0.79, 10}})
	alpha.Metadata = map[string]any{"access_token": "sk-ant-oat01-alpha-secret", "refresh_token": "sk-ant-ort01-alpha-secret", "email": "alpha@example.com"}

	bravo := readingsFileAuth("claude-bravo.json", "claude", "idx-bravo")
	store.Put(bravo.ID, "claude", []quotareading.Window{
		window("5h", quotareading.KindShort, "", 0, 4*time.Hour, 5*time.Hour, 20*time.Minute, quotareading.SourceUsage),
		window("7d", quotareading.KindLong, "", 40, 48*time.Hour, week, 20*time.Minute, quotareading.SourceUsage),
		window("7d:fable", quotareading.KindScoped, "fable", 100, 72*time.Hour, week, 20*time.Minute, quotareading.SourceUsage),
	})

	charlie := readingsHeaderAuth("claude-charlie.json", "idx-charlie", time.Minute, map[string][2]float64{"5h": {0.99, 1}, "7d": {0.1, 100}})
	delta := readingsFileAuth("claude-delta.json", "claude", "idx-delta")
	echo := readingsHeaderAuth("claude-echo.json", "idx-echo", 5*time.Minute, map[string][2]float64{"7d": {0.5, 5}})
	echo.Attributes = map[string]string{"priority": "-1"}
	foxtrot := readingsHeaderAuth("claude-foxtrot.json", "idx-foxtrot", time.Minute, map[string][2]float64{"7d": {0.1, 1}})
	foxtrot.Disabled = true
	foxtrot.Status = coreauth.StatusDisabled
	golf := readingsHeaderAuth("claude-golf.json", "idx-golf", 5*time.Minute, map[string][2]float64{"7d": {0.2, 2}})
	golf.Unavailable = true
	golf.NextRetryAfter = now.Add(30 * time.Minute)

	hotel := readingsFileAuth("claude-hotel.json", "claude", "idx-hotel")
	store.Put(hotel.ID, "claude", []quotareading.Window{
		window("7d", quotareading.KindLong, "", 100, -time.Hour, week, 3*time.Hour, quotareading.SourceUsage),
	})

	// The weekly window is spent: india is gated until it resets, whatever its 5h window says.
	india := readingsFileAuth("claude-india.json", "claude", "idx-india")
	store.Put(india.ID, "claude", []quotareading.Window{
		window("5h", quotareading.KindShort, "", 10, 3*time.Hour, 5*time.Hour, 10*time.Minute, quotareading.SourcePoll),
		window("7d", quotareading.KindLong, "", 100, 30*time.Hour, week, 10*time.Minute, quotareading.SourcePoll),
	})

	// The most urgent claude credential of all, but its 5h window is close to
	// running out: new sessions go to every credential with headroom first,
	// the one without readings included.
	juliet := readingsHeaderAuth("claude-juliet.json", "idx-juliet", 2*time.Minute, map[string][2]float64{"5h": {0.85, 1}, "7d": {0.5, 5}})
	// Its weekly window is above the gate but within the long headroom.
	kilo := readingsFileAuth("claude-kilo.json", "claude", "idx-kilo")
	store.Put(kilo.ID, "claude", []quotareading.Window{
		window("5h", quotareading.KindShort, "", 30, 3*time.Hour, 5*time.Hour, 15*time.Minute, quotareading.SourceUsage),
		window("7d", quotareading.KindLong, "", 96, 20*time.Hour, week, 15*time.Minute, quotareading.SourceUsage),
	})

	codexOne := readingsFileAuth("codex-one.json", "codex", "idx-codex-one")
	codexOne.Quota = coreauth.QuotaState{ObservedAt: now.Add(-2 * time.Minute), Signals: map[string]string{
		"X-Codex-Primary-Used-Percent":     "30",
		"X-Codex-Primary-Window-Minutes":   "300",
		"X-Codex-Primary-Reset-At":         strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10),
		"X-Codex-Secondary-Used-Percent":   "40",
		"X-Codex-Secondary-Window-Minutes": "10080",
		"X-Codex-Secondary-Reset-At":       strconv.FormatInt(now.Add(60*time.Hour).Unix(), 10),
	}}
	codexTwo := readingsFileAuth("codex-two.json", "codex", "idx-codex-two")
	store.Put(codexTwo.ID, "codex", []quotareading.Window{
		window("primary", quotareading.KindShort, "", 100, 30*time.Minute, 5*time.Hour, 4*time.Minute, quotareading.SourcePoll),
		window("secondary", quotareading.KindLong, "", 20, 100*time.Hour, week, 4*time.Minute, quotareading.SourcePoll),
	})

	apiKey := &coreauth.Auth{ID: "gemini:apikey:0123abcd", Provider: "gemini", Index: "idx-gemini", Attributes: map[string]string{"api_key": "AIza-secret-key"}}

	auths := []*coreauth.Auth{golf, kilo, codexTwo, apiKey, india, hotel, foxtrot, echo, delta, juliet, charlie, bravo, codexOne, alpha, nil}
	return auths, store
}

// readingsTestThresholds are the routing config defaults.
var readingsTestThresholds = quotareading.Thresholds{Gate: 2, HeadroomShort: 20, HeadroomLong: 5}

func TestRoutingQuotaReadingsGolden(t *testing.T) {
	auths, store := readingsGoldenFixture()
	response := buildRoutingQuotaReadings(" EF ", readingsTestThresholds, auths, store, routingfocus.NewStore(), nil, readingsTestNow)
	got, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != routingQuotaReadingsGolden {
		t.Fatalf("quota readings JSON mismatch\n got:\n%s\nwant:\n%s", got, routingQuotaReadingsGolden)
	}
	for _, secret := range []string{"secret", "alpha@example.com"} {
		if strings.Contains(string(got), secret) {
			t.Fatalf("quota readings leaked %q", secret)
		}
	}
}

// A focused credential is ranked like the selector picks it: first when it is
// in its provider's ranked group, and rank 0 like any other credential outside
// it (gated, unavailable, or below the top tier).
func TestRoutingQuotaReadingsFocus(t *testing.T) {
	t.Parallel()

	// The focus store's own clock is the wall clock, so ends are set from it;
	// the readings are evaluated at readingsTestNow, before every end.
	until := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	tests := []struct {
		name  string
		focus []routingfocus.Focus
		// want lists auth_id=rank for the credentials it names; focused names
		// the credentials reported as focused.
		want    map[string]int
		focused []string
	}{
		{
			name:    "nothing focused",
			want:    map[string]int{"claude-alpha.json": 1, "claude-bravo.json": 2, "codex-one.json": 1},
			focused: nil,
		},
		{
			name:    "a usable focus is rank 1",
			focus:   []routingfocus.Focus{{AuthID: "claude-bravo.json", Provider: "claude"}},
			want:    map[string]int{"claude-bravo.json": 1, "claude-alpha.json": 2, "claude-hotel.json": 3, "claude-delta.json": 4},
			focused: []string{"claude-bravo.json"},
		},
		{
			name:    "a focus without readings is rank 1",
			focus:   []routingfocus.Focus{{AuthID: "claude-delta.json", Provider: "claude", Until: until}},
			want:    map[string]int{"claude-delta.json": 1, "claude-alpha.json": 2, "claude-bravo.json": 3, "claude-hotel.json": 4},
			focused: []string{"claude-delta.json"},
		},
		{
			// Headroom only orders the credentials outside a focus: a usable focus
			// without it takes every request all the same.
			name:    "a usable focus without headroom is rank 1",
			focus:   []routingfocus.Focus{{AuthID: "claude-juliet.json", Provider: "claude"}},
			want:    map[string]int{"claude-juliet.json": 1, "claude-alpha.json": 2, "claude-bravo.json": 3, "claude-hotel.json": 4, "claude-delta.json": 5, "claude-kilo.json": 6},
			focused: []string{"claude-juliet.json"},
		},
		{
			name:    "a gated focus keeps rank 0",
			focus:   []routingfocus.Focus{{AuthID: "claude-charlie.json", Provider: "claude"}},
			want:    map[string]int{"claude-charlie.json": 0, "claude-alpha.json": 1},
			focused: []string{"claude-charlie.json"},
		},
		{
			name:    "a focus below the top tier keeps rank 0",
			focus:   []routingfocus.Focus{{AuthID: "claude-echo.json", Provider: "claude"}},
			want:    map[string]int{"claude-echo.json": 0, "claude-alpha.json": 1},
			focused: []string{"claude-echo.json"},
		},
		{
			name:    "an unavailable focus keeps rank 0",
			focus:   []routingfocus.Focus{{AuthID: "claude-golf.json", Provider: "claude"}},
			want:    map[string]int{"claude-golf.json": 0, "claude-alpha.json": 1},
			focused: []string{"claude-golf.json"},
		},
		{
			name: "focus is per provider",
			focus: []routingfocus.Focus{
				{AuthID: "codex-two.json", Provider: "codex"},
				{AuthID: "claude-hotel.json", Provider: "claude"},
			},
			want:    map[string]int{"claude-hotel.json": 1, "claude-alpha.json": 2, "codex-two.json": 0, "codex-one.json": 1},
			focused: []string{"claude-hotel.json", "codex-two.json"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			auths, store := readingsGoldenFixture()
			focus := routingfocus.NewStore()
			for _, f := range tc.focus {
				if err := focus.Set(f); err != nil {
					t.Fatal(err)
				}
			}
			response := buildRoutingQuotaReadings("expiring-first", readingsTestThresholds, auths, store, focus, nil, readingsTestNow)
			focusedSet := make(map[string]bool, len(tc.focused))
			for _, id := range tc.focused {
				focusedSet[id] = true
			}
			seen := 0
			for _, credential := range response.Credentials {
				if want, ok := tc.want[credential.AuthID]; ok {
					seen++
					if credential.Rank != want {
						t.Fatalf("%s rank = %d, want %d", credential.AuthID, credential.Rank, want)
					}
				}
				if credential.Focused != focusedSet[credential.AuthID] {
					t.Fatalf("%s focused = %t", credential.AuthID, credential.Focused)
				}
				var wantUntil *time.Time
				for _, f := range tc.focus {
					if f.AuthID == credential.AuthID && !f.Until.IsZero() {
						wantUntil = &f.Until
					}
				}
				if (credential.FocusUntil == nil) != (wantUntil == nil) || (wantUntil != nil && !credential.FocusUntil.Equal(*wantUntil)) {
					t.Fatalf("%s focus_until = %v, want %v", credential.AuthID, credential.FocusUntil, wantUntil)
				}
			}
			if seen != len(tc.want) {
				t.Fatalf("found %d of the credentials %v", seen, tc.want)
			}
		})
	}

	// The JSON names the fields, with the end in UTC and null without one.
	auths, store := readingsGoldenFixture()
	focus := routingfocus.NewStore()
	if err := focus.Set(routingfocus.Focus{AuthID: "claude-bravo.json", Provider: "claude", Until: until.In(time.FixedZone("CEST", 2*3600))}); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(buildRoutingQuotaReadings("expiring-first", readingsTestThresholds, auths, store, focus, nil, readingsTestNow))
	if err != nil {
		t.Fatal(err)
	}
	wantBravo := `"auth_id":"claude-bravo.json","auth_index":"idx-bravo","provider":"claude","label":"claude-bravo.json","priority":0,"usable":true,"gate_reason":"","headroom":true,"urgency_per_hour":1.25,"rank":1,"focused":true,"focus_until":"` + until.Format(time.RFC3339) + `"`
	if !strings.Contains(string(body), wantBravo) || !strings.Contains(string(body), `"rank":2,"focused":false,"focus_until":null`) {
		t.Fatalf("readings JSON = %s", body)
	}
}

// A credential that failed transiently a moment ago is ranked after the others,
// focused or not, as the selector places new sessions elsewhere until then; the
// JSON says until when.
func TestRoutingQuotaReadingsRankTransientlyFailingCredentialsLast(t *testing.T) {
	t.Parallel()
	until := readingsTestNow.Add(time.Minute).UTC()
	failing := func(authID string) (time.Time, bool) {
		if authID == "claude-alpha.json" {
			return until, true
		}
		return time.Time{}, false
	}
	ranks := func(focus *routingfocus.Store) map[string]routingQuotaReadingsCredential {
		t.Helper()
		auths, store := readingsGoldenFixture()
		response := buildRoutingQuotaReadings("expiring-first", readingsTestThresholds, auths, store, focus, failing, readingsTestNow)
		byID := make(map[string]routingQuotaReadingsCredential, len(response.Credentials))
		for _, credential := range response.Credentials {
			byID[credential.AuthID] = credential
		}
		return byID
	}

	byID := ranks(routingfocus.NewStore())
	if got := byID["claude-bravo.json"].Rank; got != 1 {
		t.Fatalf("claude-bravo.json rank = %d, want 1 while claude-alpha.json is failing", got)
	}
	alpha := byID["claude-alpha.json"]
	if alpha.Rank != 6 || alpha.AvoidedUntil == nil || !alpha.AvoidedUntil.Equal(until) {
		t.Fatalf("claude-alpha.json rank %d, avoided_until %v; want rank 6 until %v", alpha.Rank, alpha.AvoidedUntil, until)
	}
	if byID["claude-bravo.json"].AvoidedUntil != nil {
		t.Fatal("a credential that did not fail reports avoided_until")
	}

	focus := routingfocus.NewStore()
	if err := focus.Set(routingfocus.Focus{AuthID: "claude-alpha.json", Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	if got := ranks(focus)["claude-alpha.json"].Rank; got != 6 {
		t.Fatalf("the failing focus has rank %d, want 6: new sessions go elsewhere", got)
	}

	auths, store := readingsGoldenFixture()
	body, err := json.Marshal(buildRoutingQuotaReadings("expiring-first", readingsTestThresholds, auths, store, routingfocus.NewStore(), failing, readingsTestNow))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"focus_until":null,"avoided_until":"`+until.Format(time.RFC3339)+`"`) {
		t.Fatalf("readings JSON = %s", body)
	}
}

// The handler serves the live pool: every non-disabled credential, the
// normalized strategy, and the same auth_index the credential list reports.
func TestGetRoutingQuotaReadingsServesManagerCredentials(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	const secret = "sk-ant-oat01-handler-secret"
	register := func(id string, disabled bool) {
		t.Helper()
		path := filepath.Join(authDir, id)
		if err := os.WriteFile(path, []byte(`{"type":"claude"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		auth := &coreauth.Auth{
			ID:         id,
			FileName:   id,
			Provider:   "claude",
			Status:     coreauth.StatusActive,
			Disabled:   disabled,
			Attributes: map[string]string{"path": path},
			Metadata:   map[string]any{"access_token": secret, "email": "user@example.com"},
			Quota: coreauth.QuotaState{ObservedAt: time.Now(), Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-7d-Utilization": "0.25",
				"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(time.Now().Add(48*time.Hour).Unix(), 10),
			}},
		}
		if disabled {
			auth.Status = coreauth.StatusDisabled
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	register("claude-readings-handler-live.json", false)
	register("claude-readings-handler-off.json", true)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir, Routing: config.RoutingConfig{Strategy: "soonest-reset"}}, manager)

	serve := func(handler gin.HandlerFunc) []byte {
		t.Helper()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		handler(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
		}
		return recorder.Body.Bytes()
	}
	body := serve(h.GetRoutingQuotaReadings)
	if strings.Contains(string(body), secret) {
		t.Fatalf("quota readings leaked the access token: %s", body)
	}
	var readings struct {
		Strategy    string    `json:"strategy"`
		GeneratedAt time.Time `json:"generated_at"`
		Credentials []struct {
			AuthID         string   `json:"auth_id"`
			AuthIndex      string   `json:"auth_index"`
			Label          string   `json:"label"`
			Usable         bool     `json:"usable"`
			Headroom       bool     `json:"headroom"`
			UrgencyPerHour *float64 `json:"urgency_per_hour"`
			Rank           int      `json:"rank"`
			Windows        []struct {
				ID               string  `json:"id"`
				RemainingPercent float64 `json:"remaining_percent"`
				Source           string  `json:"source"`
			} `json:"windows"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(body, &readings); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if readings.Strategy != "expiring-first" || readings.GeneratedAt.IsZero() || len(readings.Credentials) != 1 {
		t.Fatalf("readings = %s", body)
	}
	credential := readings.Credentials[0]
	if credential.AuthID != "claude-readings-handler-live.json" || credential.Label != credential.AuthID || !credential.Usable || !credential.Headroom ||
		credential.Rank != 1 || credential.UrgencyPerHour == nil || len(credential.Windows) != 1 ||
		credential.Windows[0].ID != "7d" || credential.Windows[0].RemainingPercent != 75 || credential.Windows[0].Source != "header" {
		t.Fatalf("credential = %+v", credential)
	}

	var files struct {
		Files []struct {
			ID        string `json:"id"`
			AuthIndex string `json:"auth_index"`
		} `json:"files"`
	}
	if err := json.Unmarshal(serve(h.ListAuthFiles), &files); err != nil {
		t.Fatal(err)
	}
	for _, file := range files.Files {
		if file.ID == credential.AuthID && file.AuthIndex != "" && file.AuthIndex == credential.AuthIndex {
			return
		}
	}
	t.Fatalf("auth_index %q does not match the credential list %+v", credential.AuthIndex, files.Files)
}

const routingQuotaReadingsGolden = `{
  "strategy": "expiring-first",
  "generated_at": "2026-10-03T21:00:00Z",
  "credentials": [
    {
      "auth_id": "claude-alpha.json",
      "auth_index": "idx-alpha",
      "provider": "claude",
      "label": "claude-alpha.json",
      "priority": 0,
      "usable": true,
      "gate_reason": "",
      "headroom": true,
      "urgency_per_hour": 2.1,
      "rank": 1,
      "focused": false,
      "focus_until": null,
      "windows": [
        {
          "id": "5h",
          "kind": "short",
          "remaining_percent": 70,
          "resets_at": "2026-10-03T23:00:00Z",
          "observed_at": "2026-10-03T20:55:00Z",
          "source": "header"
        },
        {
          "id": "7d",
          "kind": "long",
          "remaining_percent": 21,
          "resets_at": "2026-10-04T07:00:00Z",
          "observed_at": "2026-10-03T20:55:00Z",
          "source": "header"
        }
      ]
    },
    {
      "auth_id": "claude-bravo.json",
      "auth_index": "idx-bravo",
      "provider": "claude",
      "label": "claude-bravo.json",
      "priority": 0,
      "usable": true,
      "gate_reason": "",
      "headroom": true,
      "urgency_per_hour": 1.25,
      "rank": 2,
      "focused": false,
      "focus_until": null,
      "windows": [
        {
          "id": "5h",
          "kind": "short",
          "remaining_percent": 100,
          "resets_at": "2026-10-04T01:00:00Z",
          "observed_at": "2026-10-03T20:40:00Z",
          "source": "usage"
        },
        {
          "id": "7d",
          "kind": "long",
          "remaining_percent": 60,
          "resets_at": "2026-10-05T21:00:00Z",
          "observed_at": "2026-10-03T20:40:00Z",
          "source": "usage"
        },
        {
          "id": "7d:fable",
          "kind": "scoped",
          "remaining_percent": 0,
          "resets_at": "2026-10-06T21:00:00Z",
          "observed_at": "2026-10-03T20:40:00Z",
          "source": "usage"
        }
      ]
    },
    {
      "auth_id": "claude-hotel.json",
      "auth_index": "idx-hotel",
      "provider": "claude",
      "label": "claude-hotel.json",
      "priority": 0,
      "usable": true,
      "gate_reason": "",
      "headroom": true,
      "urgency_per_hour": 0.6,
      "rank": 3,
      "focused": false,
      "focus_until": null,
      "windows": [
        {
          "id": "7d",
          "kind": "long",
          "remaining_percent": 100,
          "resets_at": "2026-10-10T20:00:00Z",
          "observed_at": "2026-10-03T18:00:00Z",
          "source": "usage"
        }
      ]
    },
    {
      "auth_id": "claude-delta.json",
      "auth_index": "idx-delta",
      "provider": "claude",
      "label": "claude-delta.json",
      "priority": 0,
      "usable": true,
      "gate_reason": "",
      "headroom": true,
      "urgency_per_hour": null,
      "rank": 4,
      "focused": false,
      "focus_until": null,
      "windows": []
    },
    {
      "auth_id": "claude-juliet.json",
      "auth_index": "idx-juliet",
      "provider": "claude",
      "label": "claude-juliet.json",
      "priority": 0,
      "usable": true,
      "gate_reason": "",
      "headroom": false,
      "headroom_reason": "5h at 15% left",
      "urgency_per_hour": 10,
      "rank": 5,
      "focused": false,
      "focus_until": null,
      "windows": [
        {
          "id": "5h",
          "kind": "short",
          "remaining_percent": 15,
          "resets_at": "2026-10-03T22:00:00Z",
          "observed_at": "2026-10-03T20:58:00Z",
          "source": "header"
        },
        {
          "id": "7d",
          "kind": "long",
          "remaining_percent": 50,
          "resets_at": "2026-10-04T02:00:00Z",
          "observed_at": "2026-10-03T20:58:00Z",
          "source": "header"
        }
      ]
    },
    {
      "auth_id": "claude-kilo.json",
      "auth_index": "idx-kilo",
      "provider": "claude",
      "label": "claude-kilo.json",
      "priority": 0,
      "usable": true,
      "gate_reason": "",
      "headroom": false,
      "headroom_reason": "7d at 4% left",
      "urgency_per_hour": 0.2,
      "rank": 6,
      "focused": false,
      "focus_until": null,
      "windows": [
        {
          "id": "5h",
          "kind": "short",
          "remaining_percent": 70,
          "resets_at": "2026-10-04T00:00:00Z",
          "observed_at": "2026-10-03T20:45:00Z",
          "source": "usage"
        },
        {
          "id": "7d",
          "kind": "long",
          "remaining_percent": 4,
          "resets_at": "2026-10-04T17:00:00Z",
          "observed_at": "2026-10-03T20:45:00Z",
          "source": "usage"
        }
      ]
    },
    {
      "auth_id": "claude-charlie.json",
      "auth_index": "idx-charlie",
      "provider": "claude",
      "label": "claude-charlie.json",
      "priority": 0,
      "usable": false,
      "gate_reason": "5h exhausted",
      "headroom": false,
      "headroom_reason": "5h at 1% left",
      "urgency_per_hour": 0.9,
      "rank": 0,
      "focused": false,
      "focus_until": null,
      "windows": [
        {
          "id": "5h",
          "kind": "short",
          "remaining_percent": 1,
          "resets_at": "2026-10-03T22:00:00Z",
          "observed_at": "2026-10-03T20:59:00Z",
          "source": "header"
        },
        {
          "id": "7d",
          "kind": "long",
          "remaining_percent": 90,
          "resets_at": "2026-10-08T01:00:00Z",
          "observed_at": "2026-10-03T20:59:00Z",
          "source": "header"
        }
      ]
    },
    {
      "auth_id": "claude-echo.json",
      "auth_index": "idx-echo",
      "provider": "claude",
      "label": "claude-echo.json",
      "priority": -1,
      "usable": true,
      "gate_reason": "",
      "headroom": true,
      "urgency_per_hour": 10,
      "rank": 0,
      "focused": false,
      "focus_until": null,
      "windows": [
        {
          "id": "7d",
          "kind": "long",
          "remaining_percent": 50,
          "resets_at": "2026-10-04T02:00:00Z",
          "observed_at": "2026-10-03T20:55:00Z",
          "source": "header"
        }
      ]
    },
    {
      "auth_id": "claude-golf.json",
      "auth_index": "idx-golf",
      "provider": "claude",
      "label": "claude-golf.json",
      "priority": 0,
      "usable": true,
      "gate_reason": "",
      "headroom": true,
      "urgency_per_hour": 40,
      "rank": 0,
      "focused": false,
      "focus_until": null,
      "windows": [
        {
          "id": "7d",
          "kind": "long",
          "remaining_percent": 80,
          "resets_at": "2026-10-03T23:00:00Z",
          "observed_at": "2026-10-03T20:55:00Z",
          "source": "header"
        }
      ]
    },
    {
      "auth_id": "claude-india.json",
      "auth_index": "idx-india",
      "provider": "claude",
      "label": "claude-india.json",
      "priority": 0,
      "usable": false,
      "gate_reason": "7d exhausted",
      "headroom": false,
      "headroom_reason": "7d at 0% left",
      "urgency_per_hour": 0,
      "rank": 0,
      "focused": false,
      "focus_until": null,
      "windows": [
        {
          "id": "5h",
          "kind": "short",
          "remaining_percent": 90,
          "resets_at": "2026-10-04T00:00:00Z",
          "observed_at": "2026-10-03T20:50:00Z",
          "source": "poll"
        },
        {
          "id": "7d",
          "kind": "long",
          "remaining_percent": 0,
          "resets_at": "2026-10-05T03:00:00Z",
          "observed_at": "2026-10-03T20:50:00Z",
          "source": "poll"
        }
      ]
    },
    {
      "auth_id": "codex-one.json",
      "auth_index": "idx-codex-one",
      "provider": "codex",
      "label": "codex-one.json",
      "priority": 0,
      "usable": true,
      "gate_reason": "",
      "headroom": true,
      "urgency_per_hour": 1,
      "rank": 1,
      "focused": false,
      "focus_until": null,
      "windows": [
        {
          "id": "primary",
          "kind": "short",
          "remaining_percent": 70,
          "resets_at": "2026-10-03T23:00:00Z",
          "observed_at": "2026-10-03T20:58:00Z",
          "source": "header"
        },
        {
          "id": "secondary",
          "kind": "long",
          "remaining_percent": 60,
          "resets_at": "2026-10-06T09:00:00Z",
          "observed_at": "2026-10-03T20:58:00Z",
          "source": "header"
        }
      ]
    },
    {
      "auth_id": "codex-two.json",
      "auth_index": "idx-codex-two",
      "provider": "codex",
      "label": "codex-two.json",
      "priority": 0,
      "usable": false,
      "gate_reason": "primary exhausted",
      "headroom": false,
      "headroom_reason": "primary at 0% left",
      "urgency_per_hour": 0.8,
      "rank": 0,
      "focused": false,
      "focus_until": null,
      "windows": [
        {
          "id": "primary",
          "kind": "short",
          "remaining_percent": 0,
          "resets_at": "2026-10-03T21:30:00Z",
          "observed_at": "2026-10-03T20:56:00Z",
          "source": "poll"
        },
        {
          "id": "secondary",
          "kind": "long",
          "remaining_percent": 80,
          "resets_at": "2026-10-08T01:00:00Z",
          "observed_at": "2026-10-03T20:56:00Z",
          "source": "poll"
        }
      ]
    },
    {
      "auth_id": "gemini:apikey:0123abcd",
      "auth_index": "idx-gemini",
      "provider": "gemini",
      "label": "gemini:apikey:0123abcd",
      "priority": 0,
      "usable": true,
      "gate_reason": "",
      "headroom": true,
      "urgency_per_hour": null,
      "rank": 1,
      "focused": false,
      "focus_until": null,
      "windows": []
    }
  ]
}`

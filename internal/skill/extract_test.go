package skill

import "testing"

// TestExtractJSONToleratesProse is the regression guard for the Phase-2 probe
// failure: claude emitted a great plan but prefixed it with Chinese prose
// ("所有事实已验证。确认：…") AND inline-code braces ("Enforcer{PerCall}"),
// then the JSON. Strict json.Unmarshal of the whole raw failed on the leading
// 所. extractJSON must return just the JSON object.
func TestExtractJSONToleratesProse(t *testing.T) {
	raw := "所有事实已验证。确认：`Enforcer{PerCall, PerTask, MaxRetries int}` 已导出。\n\n{\"plan\":[],\"risks\":[]}\n"
	got := extractJSON(raw)
	want := "{\"plan\":[],\"risks\":[]}"
	if got != want {
		t.Fatalf("extractJSON:\n got=%q\nwant=%q", got, want)
	}
}

func TestExtractJSONPureJSONPassesThrough(t *testing.T) {
	raw := `{"passed":true,"reason":"ok","failing_criteria":[]}`
	if got := extractJSON(raw); got != raw {
		t.Fatalf("pure JSON should pass through unchanged; got %q", got)
	}
}

func TestExtractJSONNoObjectReturnsRaw(t *testing.T) {
	raw := "no json object here at all"
	if got := extractJSON(raw); got != raw {
		t.Fatalf("no JSON object ⇒ return raw unchanged so ParseJSON errors clearly; got %q", got)
	}
}

package skill

import (
	"bytes"
	"encoding/json"
	"strings"
	"text/template"
)

func render(tmpl string, input any) (string, error) {
	t, err := template.New("s").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, input); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// extractJSON finds the first valid JSON object in raw and returns its
// substring. LLMs often wrap JSON in prose despite "JSON only" instructions
// (e.g. "所有事实已验证。确认：…\n\n{…}"), which makes a strict json.Unmarshal of
// the whole raw fail on the leading text (Phase-2 probe blocked here: the plan
// was excellent but prefixed with Chinese prose). This scans for the first '{'
// from which a JSON value decodes and returns that object's text. Prose that
// contains braces (e.g. "Enforcer{PerCall…}") is skipped because it is not
// valid JSON. If no valid object is found, raw is returned unchanged so the
// caller's ParseJSON reports the original error.
func extractJSON(raw string) string {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '{' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(raw[i:]))
		var v any
		if err := dec.Decode(&v); err == nil {
			return raw[i : i+int(dec.InputOffset())]
		}
	}
	return raw
}

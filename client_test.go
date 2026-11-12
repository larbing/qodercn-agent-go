package qoder

import "testing"

// resolveModel guards SetModel against values the CLI would silently
// accept and then fail every later query on (invalid model), and maps
// display names / case variants to the canonical catalog value.
func TestResolveModel(t *testing.T) {
	catalog := []map[string]any{
		{"value": "qmodel", "modelId": "qmodel", "displayName": "Qwen3.8-Flash", "isEnabled": true},
		{"value": "gmodel", "modelId": "gmodel", "displayName": "GLM-5.3", "isEnabled": true},
		{"value": "dmodel", "modelId": "dmodel", "displayName": "DeepSeek-V4-Pro", "isEnabled": true},
	}

	cases := []struct {
		model  string
		want   string
		wantOK bool
	}{
		{"Qwen3.8-Flash", "qmodel", true},   // displayName -> canonical
		{"qwen3.8-flash", "qmodel", true},   // case variant -> canonical
		{"gmodel", "gmodel", true},          // value passthrough
		{"GLM-5.3", "gmodel", true},         // displayName -> canonical
		{"DeepSeek-V4-Pro", "dmodel", true}, // displayName -> canonical
		{"DMODEL", "dmodel", true},          // modelId, case-insensitive
		{"claude-opus", "", false},          // unknown
		{"qmodel_latest", "", false},        // plausible-looking but absent
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := resolveModelName(tc.model, catalog)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("resolveModelName(%q) = (%q, %v), want (%q, %v)", tc.model, got, ok, tc.want, tc.wantOK)
		}
	}

	// Empty catalog (older CLIs) passes anything through unchanged —
	// no false guard.
	got, ok := resolveModelName("anything", nil)
	if got != "anything" || !ok {
		t.Errorf("empty catalog: got (%q, %v), want passthrough", got, ok)
	}
}

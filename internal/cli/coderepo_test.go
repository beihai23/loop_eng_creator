package cli

import "testing"

// TestParseGitHubOwnerName pins the channel-agnostic code-repo derivation: from
// any GitHub remote URL shape (SSH/HTTPS, ±.git, with an embedded token, and the
// unusual ":/" SCP-lite form) extract owner/name; reject non-GitHub / malformed.
// Landing targets this code repo regardless of the task channel (GitHub vs Linear).
func TestParseGitHubOwnerName(t *testing.T) {
	cases := map[string]string{
		"git@github.com:beihai23/loop_eng_creator.git":             "beihai23/loop_eng_creator",
		"git@github.com:/beihai23/loop_eng_creator.git":            "beihai23/loop_eng_creator", // ":/" SCP-lite (this repo's actual form)
		"git@github.com:beihai23/loop_eng_creator":                 "beihai23/loop_eng_creator",
		"https://github.com/beihai23/loop_eng_creator.git":         "beihai23/loop_eng_creator",
		"https://github.com/beihai23/loop_eng_creator":             "beihai23/loop_eng_creator",
		"https://x:token@github.com/beihai23/loop_eng_creator.git": "beihai23/loop_eng_creator",
		"ssh://git@github.com/beihai23/loop_eng_creator.git":       "beihai23/loop_eng_creator",
		// rejections:
		"git@gitlab.com:beihai23/loop_eng_creator.git": "", // not github
		"https://github.com/a/b/c":                     "", // too many slashes
		"/Users/local/repo":                            "", // local path, no github
		"git@github.com:solo":                          "", // no name (no slash)
	}
	for in, want := range cases {
		if got := parseGitHubOwnerName(in); got != want {
			t.Errorf("parseGitHubOwnerName(%q) = %q, want %q", in, got, want)
		}
	}
}

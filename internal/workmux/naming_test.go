package workmux

import "testing"

func TestWorkspaceSlugASCII(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"My Test String!!!1!1", "my-test-string-1-1"},
		{"  --test_-_cool", "test-cool"},
		{"user@example.com", "user-example-com"},
		{"feature/auth/oauth", "feature-auth-oauth"},
		{"Release_42.1", "release-42-1"},
		{"", ""},
		{"!!!", ""},
	} {
		t.Run(test.input, func(t *testing.T) {
			if got := workspaceSlug(test.input); got != test.want {
				t.Fatalf("workspaceSlug(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

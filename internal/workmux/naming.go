package workmux

import "strings"

func workspaceSlug(name string) string {
	var slug strings.Builder
	separator := true
	for _, r := range name {
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			slug.WriteByte(byte(r))
			separator = false
		} else if !separator {
			slug.WriteByte('-')
			separator = true
		}
	}
	return strings.TrimSuffix(slug.String(), "-")
}

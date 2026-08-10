package gitlab

import (
	"strings"
)

// labelVariants returns every entry of labels whose lowercased form
// equals lowered, in input order. It is how the transition removes a
// case-variant residue in the same request that applies the swap.
func labelVariants(labels []string, lowered string) []string {
	var out []string
	for _, l := range labels {
		if strings.ToLower(l) == lowered {
			out = append(out, l)
		}
	}
	return out
}

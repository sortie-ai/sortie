package issuekit

import (
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/typeutil"
)

// PriorityFromLabels returns the smallest declared priority among labels,
// or nil when none match. Both labels and priorities use lowercase names;
// callers normalize labels with [NormalizeLabels] and parse the map with
// [ParsePriorityLabels]. Names match exactly, with no glob expansion.
func PriorityFromLabels(labels []string, priorities map[string]int) *int {
	var priority *int
	for _, label := range labels {
		if value, ok := priorities[label]; ok && (priority == nil || value < *priority) {
			priority = new(value)
		}
	}
	return priority
}

// ParsePriorityLabels reads an operator's label-to-integer map, lowercasing
// its keys. It rejects blank names, names repeated after lowercasing, and
// values outside the domain's integer range. A nil value means no mapping.
func ParsePriorityLabels(raw any) (map[string]int, error) {
	if raw == nil {
		return nil, nil
	}
	var entries map[string]any
	switch m := raw.(type) {
	case map[string]any:
		entries = m
	case map[string]int:
		entries = make(map[string]any, len(m))
		for key, value := range m {
			entries[key] = value
		}
	default:
		return nil, fmt.Errorf("priority_labels: expected mapping, got %s", typeutil.DescribeYAMLType(raw))
	}
	priorities := make(map[string]int, len(entries))
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("priority_labels: label names must not be empty or blank")
		}
		lowered := strings.ToLower(name)
		if _, exists := priorities[lowered]; exists {
			return nil, fmt.Errorf("priority_labels: label %q is repeated after lowercasing", lowered)
		}
		value := entries[name]
		switch value.(type) {
		case int, int64, uint64:
		default:
			return nil, fmt.Errorf("priority_labels: label %q: expected integer, got %s", name, typeutil.DescribeYAMLType(value))
		}
		parsed, err := strconv.ParseInt(fmt.Sprint(value), 10, strconv.IntSize)
		if err != nil {
			return nil, fmt.Errorf("priority_labels: label %q: value is outside the range an integer priority accepts: %w", name, err)
		}
		priorities[lowered] = int(parsed)
	}
	return priorities, nil
}

// WarnUnresolvedPriorityLabels warns once for each declared priority label
// absent from known. Catalog names are compared case-insensitively, just as
// the issue normalizers compare them.
func WarnUnresolvedPriorityLabels(log *slog.Logger, priorities map[string]int, known []string) {
	labels := make(map[string]bool, len(known))
	for _, name := range known {
		labels[strings.ToLower(name)] = true
	}
	for _, name := range slices.Sorted(maps.Keys(priorities)) {
		if !labels[name] {
			log.Warn("priority_labels label does not resolve against repository labels", slog.String("label", name))
		}
	}
}

// Package issuekit provides shared issue normalization helpers for integration adapters.
package issuekit

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
)

// SourceComment is the adapter-local staging shape for comment normalization.
type SourceComment struct {
	ID        string
	Author    string
	Body      string
	CreatedAt string
}

// NormalizeLabels lowercases labels in input order and always returns a non-nil slice.
func NormalizeLabels(in []string) []string {
	if in == nil {
		return []string{}
	}

	out := make([]string, len(in))
	for i, label := range in {
		out[i] = strings.ToLower(label)
	}
	return out
}

// ParsePriorityIntStrict parses a JSON integer literal and returns nil for all other JSON values.
func ParsePriorityIntStrict(raw json.RawMessage) *int {
	text := strings.TrimSpace(string(raw))
	if !isJSONIntLiteral(text) {
		return nil
	}
	return parseInt(text)
}

// ParsePriorityIntFromString parses a base-10 integer string and returns nil for invalid input.
func ParsePriorityIntFromString(s string) *int {
	text := strings.TrimSpace(s)
	if !isBase10Integer(text) {
		return nil
	}
	return parseInt(text)
}

// NormalizeComments maps adapter-local comment values to [domain.Comment] values.
func NormalizeComments(in []SourceComment) []domain.Comment {
	if in == nil {
		return []domain.Comment{}
	}

	out := make([]domain.Comment, len(in))
	for i, comment := range in {
		out[i] = domain.Comment{
			ID:        comment.ID,
			Author:    comment.Author,
			Body:      comment.Body,
			CreatedAt: comment.CreatedAt,
		}
	}
	return out
}

func isJSONIntLiteral(s string) bool {
	if !isBase10Integer(s) {
		return false
	}
	if s[0] == '-' {
		return len(s) <= 2 || s[1] != '0'
	}
	return len(s) <= 1 || s[0] != '0'
}

func isBase10Integer(s string) bool {
	if s == "" {
		return false
	}

	start := 0
	if s[0] == '-' {
		start = 1
	}
	if start == len(s) {
		return false
	}

	for i := start; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func parseInt(s string) *int {
	value, err := strconv.ParseInt(s, 10, strconv.IntSize)
	if err != nil {
		return nil
	}
	parsed := int(value)
	return &parsed
}

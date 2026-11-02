package commentfeed

import (
	"encoding/json"
	"strings"
)

const (
	earlierIntro = "## Issue comments from earlier agent sessions\n\n" +
		"An earlier agent session on this issue was given these comments; this session has not seen them. " +
		"Each shows its current text, oldest first. Each comment body is a JSON string."
	newIntro = "## New issue comments\n\n" +
		"Comments on this issue you have not been given yet, oldest first. Each comment body is a JSON string."
	replacingMark       = " (edited: this text replaces the version you were given earlier)"
	unknownAuthorHeader = "unknown author"
)

// Place renders a prompt for sel and returns it with the comments it places,
// Earlier first. It renders the prompt with the feed views emptied and filled;
// when the filled render is the template's own, non-blank, and differs from the
// emptied one (or the emptied one fails), the template placed the comments and
// the filled render is returned alone, otherwise the feed block follows it. A
// render reports builtin when it substituted a built-in prompt for the
// template's output, which never shows the feed. An empty sel renders once and
// places nothing, and a failing filled render places nothing.
//
// A render function must have no effect beyond its result and the side
// outputs it assigns on every call, so the side outputs of the last call
// match the returned prompt.
func Place(render func(Selection) (prompt string, builtin bool, err error), sel Selection) (prompt string, placed []Pending, err error) {
	if len(sel.Earlier) == 0 && len(sel.New) == 0 {
		prompt, _, err = render(sel)
		if err != nil {
			return "", nil, err
		}
		return prompt, nil, nil
	}
	emptied, _, emptiedErr := render(Selection{})
	filled, filledBuiltin, err := render(sel)
	if err != nil {
		return "", nil, err
	}
	placed = make([]Pending, 0, len(sel.Earlier)+len(sel.New))
	placed = append(placed, sel.Earlier...)
	placed = append(placed, sel.New...)
	if !filledBuiltin && strings.TrimSpace(filled) != "" && (emptiedErr != nil || filled != emptied) {
		return filled, placed, nil
	}
	return filled + "\n\n" + Block(sel), placed, nil
}

// Block renders the feed block for sel: the earlier group, then the new
// group, each only when non-empty. Each body is a JSON string, encoded as the
// prompt template function toJSON encodes it.
func Block(sel Selection) string {
	var groups []string
	if len(sel.Earlier) > 0 {
		groups = append(groups, group(earlierIntro, sel.Earlier))
	}
	if len(sel.New) > 0 {
		groups = append(groups, group(newIntro, sel.New))
	}
	return strings.Join(groups, "\n\n")
}

func group(intro string, comments []Pending) string {
	parts := make([]string, 0, 1+2*len(comments))
	parts = append(parts, intro)
	for _, p := range comments {
		author := p.Comment.Author
		if author == "" {
			author = unknownAuthorHeader
		}
		header := "### " + author + ", " + p.Comment.CreatedAt
		if p.Replacing {
			header += replacingMark
		}
		parts = append(parts, header, "<issue_comment>"+quoteBody(p.Comment.Body)+"</issue_comment>")
	}
	return strings.Join(parts, "\n\n")
}

// quoteBody matches the toJSON template function so the block and the
// operator's own quoting agree. Marshaling a string cannot fail.
func quoteBody(body string) string {
	quoted, _ := json.Marshal(body)
	return string(quoted)
}

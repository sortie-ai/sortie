package commentfeed

import "strings"

const (
	earlierIntro = "## Issue comments from earlier agent sessions\n\n" +
		"An earlier agent session on this issue was given these comments; this session has not seen them. " +
		"Each shows its current text, oldest first."
	newIntro = "## New issue comments\n\n" +
		"Comments on this issue you have not been given yet, oldest first."
	replacingMark       = " (edited: this text replaces the version you were given earlier)"
	unknownAuthorHeader = "unknown author"
)

// Place renders a prompt and appends the feed block for sel after it. It
// returns the output of its last render call and the comments the block
// carries, Earlier first; an empty sel leaves the render unchanged and places
// nothing, and a render error places nothing.
//
// A render function must have no effect beyond its result and the side
// outputs it assigns on every call, so the side outputs of the last call
// match the returned prompt.
func Place(render func(Selection) (string, error), sel Selection) (prompt string, placed []Pending, err error) {
	prompt, err = render(sel)
	if err != nil {
		return "", nil, err
	}
	if len(sel.Earlier) == 0 && len(sel.New) == 0 {
		return prompt, nil, nil
	}
	placed = make([]Pending, 0, len(sel.Earlier)+len(sel.New))
	placed = append(placed, sel.Earlier...)
	placed = append(placed, sel.New...)
	return prompt + "\n\n" + Block(sel), placed, nil
}

// Block renders the feed block for sel: the earlier group, then the new
// group, each only when non-empty. Comment text appears as read.
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
		parts = append(parts, header, p.Comment.Body)
	}
	return strings.Join(parts, "\n\n")
}

package prompt

import (
	"bytes"
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"text/template/parse"
	"unicode"
)

// PartialFile is one partial as the workflow loader read it.
type PartialFile struct {
	// Path identifies the file and is the Source of every diagnostic about
	// it.
	Path string

	// Name is the parse name of the file's define blocks.
	Name string

	// Body is the file content without a leading UTF-8 byte-order mark.
	Body string
}

// defineBlock is a named template that a {{ define }} or {{ block }}
// action introduces.
type defineBlock struct {
	name string
	line int
	tree *parse.Tree
}

// partialBlock is a define block together with the partial that holds it.
type partialBlock struct {
	defineBlock
	path string
	body string
}

// Partials holds the define blocks of one load's partials. A nil value
// holds none. It is read-only once built and safe for concurrent use.
type Partials struct {
	blocks []partialBlock
	index  map[string]int
	paths  map[string]string
}

// owner returns the block that defines name.
func (p *Partials) owner(name string) (partialBlock, bool) {
	if p == nil {
		return partialBlock{}, false
	}
	i, ok := p.index[name]
	if !ok {
		return partialBlock{}, false
	}
	return p.blocks[i], true
}

// blockList returns every define block in partial load order, and in line
// order within a partial.
func (p *Partials) blockList() []partialBlock {
	if p == nil {
		return nil
	}
	return p.blocks
}

// pathOf returns the Path of the partial whose parse name is parseName.
func (p *Partials) pathOf(parseName string) (string, bool) {
	if p == nil {
		return "", false
	}
	path, ok := p.paths[parseName]
	return path, ok
}

// parseNames returns the parse name of every partial.
func (p *Partials) parseNames() []string {
	if p == nil {
		return nil
	}
	names := make([]string, 0, len(p.paths))
	for name := range p.paths {
		names = append(names, name)
	}
	return names
}

// ParsePartials compiles every partial alone and collects their define
// blocks. A fault in a file returns a [*TemplateError] with Kind
// [ErrTemplateParse] located in that file. A partial may call a name that
// only a prompt template defines, so calls are checked later, by
// [ParseWithPartials].
func ParsePartials(files []PartialFile) (*Partials, error) {
	p := &Partials{
		index: make(map[string]int),
		paths: make(map[string]string, len(files)),
	}
	for _, f := range files {
		blocks, err := parsePartialFile(f)
		if err != nil {
			return nil, err
		}
		for _, b := range blocks {
			if b.name == mainTemplateName {
				return nil, parseError(f.Path, b.line, "template name %q is reserved for the prompt template", b.name)
			}
			if prev, dup := p.owner(b.name); dup {
				return nil, parseError(f.Path, b.line, "template %q is already defined in %s", b.name, prev.path)
			}
			p.index[b.name] = len(p.blocks)
			p.blocks = append(p.blocks, partialBlock{defineBlock: b, path: f.Path, body: f.Body})
		}
		p.paths[f.Name] = f.Path
	}
	return p, nil
}

func parsePartialFile(f PartialFile) ([]defineBlock, error) {
	set, err := newSet(f.Name).Parse(f.Body)
	if err != nil {
		return nil, &TemplateError{
			Kind:   ErrTemplateParse,
			Source: f.Path,
			Line:   leadingLine(err, f.Name),
			Err:    err,
		}
	}
	// A partial's own text is never callable, so text left outside the
	// define blocks would vanish from every prompt without notice.
	if line, found := firstTextOutsideDefines(set.Tree); found {
		return nil, parseError(f.Path, line, "only define blocks, white space, and comments may appear outside a define block")
	}
	return defineBlocks(set, f.Name, f.Body), nil
}

func firstTextOutsideDefines(tree *parse.Tree) (int, bool) {
	for _, n := range tree.Root.Nodes {
		if parse.IsEmptyTree(n) {
			continue
		}
		line := lineOf(tree, n)
		if text, ok := n.(*parse.TextNode); ok {
			// A text node starts at the end of the previous action, so the
			// stray text itself may sit on a later line.
			leading := text.Text[:len(text.Text)-len(bytes.TrimLeftFunc(text.Text, unicode.IsSpace))]
			line += bytes.Count(leading, []byte("\n"))
		}
		return line, true
	}
	return 0, false
}

// checkCalls fails on the first call, in either branch of every if, range
// and with, that names no define block of the set, and records every name
// a call reaches. A call cycle ends on the visited set; the executor's
// depth limit ends an endless render.
func (t *Template) checkCalls() error {
	reached := make(map[string]struct{})
	var visit func(tree *parse.Tree, node parse.Node) error
	visitBranch := func(tree *parse.Tree, b *parse.BranchNode) error {
		if err := visit(tree, b.List); err != nil {
			return err
		}
		return visit(tree, b.ElseList)
	}
	visit = func(tree *parse.Tree, node parse.Node) error {
		switch n := node.(type) {
		case *parse.ListNode:
			if n == nil {
				return nil
			}
			for _, child := range n.Nodes {
				if err := visit(tree, child); err != nil {
					return err
				}
			}
		case *parse.IfNode:
			return visitBranch(tree, &n.BranchNode)
		case *parse.RangeNode:
			return visitBranch(tree, &n.BranchNode)
		case *parse.WithNode:
			return visitBranch(tree, &n.BranchNode)
		case *parse.TemplateNode:
			if _, seen := reached[n.Name]; seen {
				return nil
			}
			callee := t.callee(n.Name)
			if callee == nil {
				return t.undefinedCall(tree, n)
			}
			reached[n.Name] = struct{}{}
			return visit(callee, callee.Root)
		}
		return nil
	}
	if err := visit(t.tmpl.Tree, t.tmpl.Root); err != nil {
		return err
	}
	t.reached = reached
	return nil
}

// callee returns the tree of the define block name. The prompt template
// itself is not callable.
func (t *Template) callee(name string) *parse.Tree {
	if name == mainTemplateName {
		return nil
	}
	if c := t.tmpl.Lookup(name); c != nil {
		return c.Tree
	}
	return nil
}

func (t *Template) undefinedCall(tree *parse.Tree, call *parse.TemplateNode) error {
	source, line := t.origin(tree.ParseName, lineOf(tree, call))
	msg := fmt.Sprintf("calls template %q, which is not defined", call.Name)
	if source != t.source {
		msg += ", reached from " + t.source
	}
	return &TemplateError{
		Kind:   ErrTemplateParse,
		Source: source,
		Line:   line,
		Err:    fmt.Errorf("%s", msg),
	}
}

func parseError(source string, line int, format string, args ...any) error {
	return &TemplateError{
		Kind:   ErrTemplateParse,
		Source: source,
		Line:   line,
		Err:    fmt.Errorf(format, args...),
	}
}

// defineBlocks lists the named templates of set other than the main one,
// ordered by line so the first fault a caller reports is the same on every
// run.
func defineBlocks(set *template.Template, mainName, text string) []defineBlock {
	lines := defineLines(text)
	var blocks []defineBlock
	for _, d := range set.Templates() {
		if d.Name() == mainName || d.Tree == nil {
			continue
		}
		line, ok := lines[d.Name()]
		if !ok {
			line = lineOf(d.Tree, d.Root)
		}
		blocks = append(blocks, defineBlock{name: d.Name(), line: line, tree: d.Tree})
	}
	slices.SortFunc(blocks, func(a, b defineBlock) int {
		return cmp.Or(cmp.Compare(a.line, b.line), strings.Compare(a.name, b.name))
	})
	return blocks
}

// defineAction matches the opening of a define or block action up to its
// quoted name.
var defineAction = regexp.MustCompile(`\{\{(?:-\s)?\s*(?:define|block)\s+("(?:[^"\\]|\\.)*"|` + "`[^`]*`" + `)`)

// defineLines maps each define name in text to the line of its first
// opening action. The parse tree of a define block records no position for
// the action itself, and the position of its body drifts past any trimmed
// white space.
func defineLines(text string) map[string]int {
	lines := make(map[string]int)
	for _, m := range defineAction.FindAllStringSubmatchIndex(text, -1) {
		name, err := strconv.Unquote(text[m[2]:m[3]])
		if err != nil {
			continue
		}
		if _, seen := lines[name]; !seen {
			lines[name] = 1 + strings.Count(text[:m[0]], "\n")
		}
	}
	return lines
}

// lineOf returns the 1-based line of node within the file tree was parsed
// from.
func lineOf(tree *parse.Tree, node parse.Node) int {
	location, _ := tree.ErrorContext(node)
	// The location reads "<parse name>:<line>:<column>" and the parse name
	// may hold colons.
	location = location[:strings.LastIndex(location, ":")]
	line, _ := strconv.Atoi(location[strings.LastIndex(location, ":")+1:])
	return line
}

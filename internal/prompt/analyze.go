package prompt

import (
	"fmt"
	"strings"
	"text/template/parse"

	"github.com/sortie-ai/sortie/internal/maputil"
)

// WarnKind classifies a template static analysis warning.
type WarnKind int

const (
	// WarnDotContext flags a FieldNode referencing a top-level data key
	// inside a range or with block where dot has been redefined. This
	// includes pipe arguments to nested range/with nodes, because those
	// arguments are evaluated in the enclosing scope where dot is already
	// the current element.
	WarnDotContext WarnKind = iota + 1

	// WarnUnknownVar flags a top-level variable reference outside the
	// recognized set. The recognized set is exactly the top-level keys
	// the renderer registers: [Template.Render] seeds them.
	WarnUnknownVar

	// WarnUnknownField flags a sub-field of a known top-level variable
	// that does not exist in the domain schema.
	WarnUnknownField
)

// TemplateWarning represents a single advisory diagnostic from template
// static analysis. These do not affect runtime behavior.
type TemplateWarning struct {
	Kind    WarnKind
	Node    string
	Message string
}

// coreKeys lists the top-level template variables [Template.Render]
// always populates, independent of [continuationKeys].
var coreKeys = []string{"issue", "attempt", "run"}

// topLevelOrder is coreKeys followed by every entry of continuationKeys,
// in that order. It fixes the enumeration order used by topLevelList.
var topLevelOrder = func() []string {
	order := make([]string, 0, len(coreKeys)+len(continuationKeys))
	order = append(order, coreKeys...)
	order = append(order, continuationKeys...)
	return order
}()

// topLevelKeys is the set of recognized top-level template variables,
// derived from topLevelOrder so a name registered with the renderer is
// recognized without a second edit here.
var topLevelKeys = func() map[string]struct{} {
	keys := make(map[string]struct{}, len(topLevelOrder))
	for _, k := range topLevelOrder {
		keys[k] = struct{}{}
	}
	return keys
}()

// topLevelList is every entry of topLevelOrder prefixed with "." and
// joined with ", ", for use in the unknown_var advisory message.
var topLevelList = func() string {
	names := make([]string, len(topLevelOrder))
	for i, k := range topLevelOrder {
		names[i] = "." + k
	}
	return strings.Join(names, ", ")
}()

// templateFieldSchema defines valid sub-fields for each top-level
// template variable. A nil value means the variable carries no
// addressable sub-fields, whether it is a scalar or a list. A map value
// enumerates the known child fields; a nil entry in the child map means
// the child is a scalar, a non-nil entry means nested fields are valid.
var templateFieldSchema = map[string]map[string]map[string]bool{
	"issue": {
		"id":          nil,
		"identifier":  nil,
		"title":       nil,
		"description": nil,
		"priority":    nil,
		"state":       nil,
		"branch_name": nil,
		"url":         nil,
		"labels":      nil,
		"assignee":    nil,
		"issue_type":  nil,
		"parent":      {"id": true, "identifier": true},
		"comments":    nil,
		"blocked_by":  nil,
		"created_at":  nil,
		"updated_at":  nil,
	},
	"run": {
		"turn_number":     nil,
		"max_turns":       nil,
		"is_continuation": nil,
	},
	"attempt": nil,
	"ci_failure": {
		"status":        nil,
		"check_runs":    {"name": true, "status": true, "conclusion": true, "details_url": true},
		"log_excerpt":   nil,
		"failing_count": nil,
		"ref":           nil,
	},
	"review_comments":     nil,
	"bot_review_comments": nil,
	"merge_conflict": {
		"pr_number": nil,
		"branch":    nil,
		"head_sha":  nil,
		"base":      nil,
	},
	"label_review": {
		"pr_number":    nil,
		"owner":        nil,
		"repo":         nil,
		"actor":        nil,
		"requested_at": nil,
	},
	"label_fix": {
		"pr_number":    nil,
		"owner":        nil,
		"repo":         nil,
		"branch":       nil,
		"actor":        nil,
		"requested_at": nil,
	},
}

// AnalyzeTemplate performs static analysis on a parsed template and
// returns advisory warnings. The analysis detects three classes of
// problems: dot-context misuse inside range/with blocks, unknown
// top-level template variables, and unknown sub-fields of known
// variables. Returns nil when no warnings are found.
func AnalyzeTemplate(t *Template) []TemplateWarning {
	if t == nil {
		return nil
	}
	tree := t.Tree()
	if tree == nil || tree.Root == nil {
		return nil
	}
	a := &analyzer{}
	a.walkNode(tree.Root, 0)
	return a.warnings
}

// analyzer accumulates warnings during a single [AnalyzeTemplate] call.
type analyzer struct {
	warnings []TemplateWarning
}

func (a *analyzer) walkNode(node parse.Node, scopeDepth int) {
	if node == nil {
		return
	}
	switch n := node.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, child := range n.Nodes {
			a.walkNode(child, scopeDepth)
		}
	case *parse.ActionNode:
		a.walkPipe(n.Pipe, scopeDepth)
	case *parse.IfNode:
		a.walkPipe(n.Pipe, scopeDepth)
		a.walkNode(n.List, scopeDepth)
		a.walkNode(n.ElseList, scopeDepth)
	case *parse.RangeNode:
		a.walkPipe(n.Pipe, scopeDepth)
		a.walkNode(n.List, scopeDepth+1)
		a.walkNode(n.ElseList, scopeDepth)
	case *parse.WithNode:
		a.walkPipe(n.Pipe, scopeDepth)
		a.walkNode(n.List, scopeDepth+1)
		a.walkNode(n.ElseList, scopeDepth)
	}
	// TextNode, CommentNode, BreakNode, ContinueNode, TemplateNode: skip.
}

func (a *analyzer) walkPipe(pipe *parse.PipeNode, scopeDepth int) {
	if pipe == nil {
		return
	}
	for _, cmd := range pipe.Cmds {
		a.walkCommand(cmd, scopeDepth)
	}
}

func (a *analyzer) walkCommand(cmd *parse.CommandNode, scopeDepth int) {
	for _, arg := range cmd.Args {
		switch n := arg.(type) {
		case *parse.FieldNode:
			a.checkFieldNode(n.Ident, scopeDepth)
		case *parse.VariableNode:
			if len(n.Ident) > 0 && n.Ident[0] == "$" {
				a.checkVariableNode(n.Ident[1:], scopeDepth)
			}
		case *parse.PipeNode:
			a.walkPipe(n, scopeDepth)
		}
	}
}

func (a *analyzer) checkFieldNode(ident []string, scopeDepth int) {
	if len(ident) == 0 {
		return
	}
	_, isTopLevel := topLevelKeys[ident[0]]

	// Dot-context misuse inside range/with. The early return is
	// intentional: when dot is redefined, the entire expression is
	// suspect, so sub-field validation is skipped. Emitting both would
	// be noise — the operator must fix the dot reference first, which
	// may change the sub-field chain entirely.
	if scopeDepth > 0 && isTopLevel {
		expr := "." + strings.Join(ident, ".")
		a.warnings = append(a.warnings, TemplateWarning{
			Kind: WarnDotContext,
			Node: expr,
			Message: fmt.Sprintf(
				"did you mean %q instead of %q? Inside a {{ range }}/{{ with }} block (including arguments to nested range/with), dot refers to the current element, not root data",
				"$"+expr, expr),
		})
		return
	}

	// Unknown top-level variable (only at scope depth 0).
	if scopeDepth == 0 && !isTopLevel {
		expr := "." + strings.Join(ident, ".")
		a.warnings = append(a.warnings, TemplateWarning{
			Kind:    WarnUnknownVar,
			Node:    expr,
			Message: fmt.Sprintf("unknown template variable %q; valid top-level variables are: %s", expr, topLevelList),
		})
		return
	}

	// Unknown sub-field of a known top-level key.
	if isTopLevel {
		a.validateFieldChain(ident, "."+strings.Join(ident, "."))
	}
}

func (a *analyzer) checkVariableNode(ident []string, scopeDepth int) {
	_ = scopeDepth // used only by checkFieldNode; kept in signature for symmetry
	if len(ident) == 0 {
		return
	}
	_, isTopLevel := topLevelKeys[ident[0]]

	// Unknown top-level via $ (scope-independent).
	if !isTopLevel {
		expr := "$." + strings.Join(ident, ".")
		a.warnings = append(a.warnings, TemplateWarning{
			Kind:    WarnUnknownVar,
			Node:    expr,
			Message: fmt.Sprintf("unknown template variable %q; valid top-level variables are: %s", expr, topLevelList),
		})
		return
	}

	// Unknown sub-field via $ chain.
	if isTopLevel {
		a.validateFieldChain(ident, "$."+strings.Join(ident, "."))
	}
}

func (a *analyzer) validateFieldChain(ident []string, nodeText string) {
	topKey := ident[0]
	schema := templateFieldSchema[topKey]

	// Scalar top-level key (e.g. "attempt") — any sub-field is invalid.
	if schema == nil && len(ident) >= 2 {
		a.warnings = append(a.warnings, TemplateWarning{
			Kind:    WarnUnknownField,
			Node:    nodeText,
			Message: fmt.Sprintf("unknown field %q; %q is a scalar with no sub-fields", nodeText, topKey),
		})
		return
	}

	if len(ident) < 2 {
		return
	}

	// Sub-field of known top-level key.
	subField := ident[1]
	nestedSchema, exists := schema[subField]
	if !exists {
		known := maputil.SortedKeys(schema)
		a.warnings = append(a.warnings, TemplateWarning{
			Kind:    WarnUnknownField,
			Node:    nodeText,
			Message: fmt.Sprintf("unknown field %q; known fields: %s", nodeText, strings.Join(known, ", ")),
		})
		return
	}

	if len(ident) < 3 {
		return
	}

	// Nested sub-field (e.g. .issue.parent.identifier).
	if nestedSchema == nil {
		a.warnings = append(a.warnings, TemplateWarning{
			Kind:    WarnUnknownField,
			Node:    nodeText,
			Message: fmt.Sprintf("unknown field %q; %q is a scalar with no sub-fields", nodeText, ident[0]+"."+subField),
		})
		return
	}

	nestedField := ident[2]
	if !nestedSchema[nestedField] {
		known := maputil.SortedKeys(nestedSchema)
		a.warnings = append(a.warnings, TemplateWarning{
			Kind:    WarnUnknownField,
			Node:    nodeText,
			Message: fmt.Sprintf("unknown field %q; known fields: %s", nodeText, strings.Join(known, ", ")),
		})
		return
	}

	// Level 3 fields are scalars in the current schema; any further
	// chaining is invalid.
	if len(ident) > 3 {
		base := ident[0] + "." + subField + "." + nestedField
		a.warnings = append(a.warnings, TemplateWarning{
			Kind:    WarnUnknownField,
			Node:    nodeText,
			Message: fmt.Sprintf("unknown field %q; %q is a scalar with no sub-fields", nodeText, base),
		})
	}
}

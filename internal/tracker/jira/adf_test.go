package jira

import (
	"encoding/json"
	"testing"
)

func TestFlattenADF(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input any
		want  string
	}{
		{
			name:  "nil input",
			input: nil,
			want:  "",
		},
		{
			name:  "string input",
			input: "not a map",
			want:  "",
		},
		{
			name:  "integer input",
			input: 42,
			want:  "",
		},
		{
			name: "simple paragraph",
			input: map[string]any{
				"type": "doc",
				"content": []any{
					map[string]any{
						"type": "paragraph",
						"content": []any{
							map[string]any{"type": "text", "text": "Hello"},
						},
					},
				},
			},
			want: "Hello",
		},
		{
			name: "two paragraphs",
			input: map[string]any{
				"type": "doc",
				"content": []any{
					map[string]any{
						"type": "paragraph",
						"content": []any{
							map[string]any{"type": "text", "text": "Hello"},
						},
					},
					map[string]any{
						"type": "paragraph",
						"content": []any{
							map[string]any{"type": "text", "text": "World"},
						},
					},
				},
			},
			want: "Hello\nWorld",
		},
		{
			name: "nested bullet list",
			input: map[string]any{
				"type": "doc",
				"content": []any{
					map[string]any{
						"type": "bulletList",
						"content": []any{
							map[string]any{
								"type": "listItem",
								"content": []any{
									map[string]any{
										"type": "paragraph",
										"content": []any{
											map[string]any{"type": "text", "text": "Item A"},
										},
									},
								},
							},
							map[string]any{
								"type": "listItem",
								"content": []any{
									map[string]any{
										"type": "paragraph",
										"content": []any{
											map[string]any{"type": "text", "text": "Item B"},
										},
									},
								},
							},
						},
					},
				},
			},
			want: "Item A\n\nItem B",
		},
		{
			name: "code block",
			input: map[string]any{
				"type": "doc",
				"content": []any{
					map[string]any{
						"type": "codeBlock",
						"content": []any{
							map[string]any{"type": "text", "text": "fmt.Println()"},
						},
					},
				},
			},
			want: "fmt.Println()",
		},
		{
			name: "unknown node type with children",
			input: map[string]any{
				"type": "doc",
				"content": []any{
					map[string]any{
						"type": "someUnknownType",
						"content": []any{
							map[string]any{
								"type": "paragraph",
								"content": []any{
									map[string]any{"type": "text", "text": "Inside"},
								},
							},
						},
					},
				},
			},
			want: "Inside",
		},
		{
			name:  "text node without content",
			input: map[string]any{"type": "text", "text": "Just text"},
			want:  "Just text",
		},
		{
			name: "empty content array",
			input: map[string]any{
				"type":    "doc",
				"content": []any{},
			},
			want: "",
		},
		{
			name: "missing content key",
			input: map[string]any{
				"type": "doc",
			},
			want: "",
		},
		{
			name: "text node with missing text field",
			input: map[string]any{
				"type": "text",
			},
			want: "",
		},
		{
			name: "multiple inline text nodes in paragraph",
			input: map[string]any{
				"type": "doc",
				"content": []any{
					map[string]any{
						"type": "paragraph",
						"content": []any{
							map[string]any{"type": "text", "text": "Hello "},
							map[string]any{"type": "text", "text": "World"},
						},
					},
				},
			},
			want: "Hello World",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := flattenADF(tt.input)
			if got != tt.want {
				t.Errorf("flattenADF() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFlattenADF_Fixture(t *testing.T) {
	t.Parallel()

	data := loadFixture(t, "adf_description.json")

	var node any
	if err := json.Unmarshal(data, &node); err != nil {
		t.Fatalf("unmarshaling fixture: %v", err)
	}

	got := flattenADF(node)
	// heading "Overview\n" + paragraph "...task.\n" + bulletList(listItem(paragraph\n)\n listItem(paragraph\n)\n)\n + codeBlock\n + unknown(paragraph\n)
	want := "Overview\nThis is the main description of the task.\nItem one\n\nItem two\n\n\necho hello\nInside unknown"
	if got != want {
		t.Errorf("flattenADF(fixture) =\n%q\nwant\n%q", got, want)
	}
}

func TestBuildADFComment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		text           string
		wantParagraphs int
		wantBodyType   string
		wantVersion    int
	}{
		{
			name:           "single line",
			text:           "Session started.",
			wantParagraphs: 1,
			wantBodyType:   "doc",
			wantVersion:    1,
		},
		{
			name:           "multi-line",
			text:           "Line one\nLine two\nLine three",
			wantParagraphs: 3,
			wantBodyType:   "doc",
			wantVersion:    1,
		},
		{
			name:           "empty string produces one empty paragraph",
			text:           "",
			wantParagraphs: 1,
			wantBodyType:   "doc",
			wantVersion:    1,
		},
		{
			name:           "trailing newline",
			text:           "Hello\n",
			wantParagraphs: 2,
			wantBodyType:   "doc",
			wantVersion:    1,
		},
		{
			name:           "blank line between paragraphs",
			text:           "Above\n\nBelow",
			wantParagraphs: 3,
			wantBodyType:   "doc",
			wantVersion:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := buildADFComment(tt.text)

			body, ok := got["body"].(map[string]any)
			if !ok {
				t.Fatal("buildADFComment() missing \"body\" key or wrong type")
			}

			if v, _ := body["version"].(int); v != tt.wantVersion {
				t.Errorf("body.version = %d, want %d", v, tt.wantVersion)
			}
			if v, _ := body["type"].(string); v != tt.wantBodyType {
				t.Errorf("body.type = %q, want %q", v, tt.wantBodyType)
			}

			content, ok := body["content"].([]any)
			if !ok {
				t.Fatal("body.content is not []any")
			}
			if len(content) != tt.wantParagraphs {
				t.Fatalf("paragraph count = %d, want %d", len(content), tt.wantParagraphs)
			}

			for i, node := range content {
				p, ok := node.(map[string]any)
				if !ok {
					t.Fatalf("paragraph[%d] is not map[string]any", i)
				}
				if pt, _ := p["type"].(string); pt != "paragraph" {
					t.Errorf("paragraph[%d].type = %q, want %q", i, pt, "paragraph")
				}
			}
		})
	}
}

func TestBuildADFComment_TextContent(t *testing.T) {
	t.Parallel()

	got := buildADFComment("Hello\n\nWorld")

	body := got["body"].(map[string]any)
	content := body["content"].([]any)

	// Three paragraphs: "Hello", empty, "World"
	if len(content) != 3 {
		t.Fatalf("paragraph count = %d, want 3", len(content))
	}

	// First paragraph: one text node with "Hello".
	p0 := content[0].(map[string]any)
	p0Content := p0["content"].([]any)
	if len(p0Content) != 1 {
		t.Fatalf("paragraph[0] content length = %d, want 1", len(p0Content))
	}
	textNode := p0Content[0].(map[string]any)
	if textNode["type"] != "text" {
		t.Errorf("paragraph[0] text node type = %q, want %q", textNode["type"], "text")
	}
	if textNode["text"] != "Hello" {
		t.Errorf("paragraph[0] text = %q, want %q", textNode["text"], "Hello")
	}

	// Second paragraph: empty content (blank line).
	p1 := content[1].(map[string]any)
	p1Content := p1["content"].([]any)
	if len(p1Content) != 0 {
		t.Errorf("paragraph[1] content length = %d, want 0 (blank line)", len(p1Content))
	}

	// Third paragraph: "World".
	p2 := content[2].(map[string]any)
	p2Content := p2["content"].([]any)
	if len(p2Content) != 1 {
		t.Fatalf("paragraph[2] content length = %d, want 1", len(p2Content))
	}
	if p2Content[0].(map[string]any)["text"] != "World" {
		t.Errorf("paragraph[2] text = %q, want %q", p2Content[0].(map[string]any)["text"], "World")
	}
}

func TestBuildADFComment_RoundTrip(t *testing.T) {
	t.Parallel()

	input := "Session dispatched\nAgent: claude-code\nAttempt: 1"

	adf := buildADFComment(input)
	body, _ := adf["body"].(map[string]any)

	flattened := flattenADF(body)
	if flattened != input {
		t.Errorf("flattenADF(buildADFComment(%q)) = %q, want round-trip identity", input, flattened)
	}
}

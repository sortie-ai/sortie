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

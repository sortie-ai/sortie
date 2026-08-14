package httpkit

import "testing"

func TestRedactURLUserinfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "username and password",
			raw:  "https://operator:secret@example.com/path?key=value#fragment",
			want: "https://example.com/path?key=value#fragment",
		},
		{
			name: "token in username position",
			raw:  "ftp://token-only@example.com/path",
			want: "ftp://example.com/path",
		},
		{
			name: "malformed URL still redacted",
			raw:  "https://operator:secret@example.com/%zz",
			want: "https://example.com/%zz",
		},
		{
			name: "scheme-relative URL",
			raw:  "//operator:secret@example.com/path",
			want: "//example.com/path",
		},
		{
			name: "last at sign delimits malformed userinfo",
			raw:  "https://operator:p@ss@example.com/path",
			want: "https://example.com/path",
		},
		{
			name: "userinfo with missing host",
			raw:  "https://operator:secret@",
			want: "https://",
		},
		{
			name: "URL without userinfo unchanged",
			raw:  "https://example.com/path",
			want: "https://example.com/path",
		},
		{
			name: "at sign outside authority unchanged",
			raw:  "https://example.com/users/operator@example.com?owner=user@example.com",
			want: "https://example.com/users/operator@example.com?owner=user@example.com",
		},
		{
			name: "userinfo without authority marker",
			raw:  "operator:secret@example.com",
			want: "example.com",
		},
		{
			name: "userinfo after single slash scheme typo",
			raw:  "https:/operator:secret@example.com/path",
			want: "https:/example.com/path",
		},
		{
			name: "at sign after path without authority marker unchanged",
			raw:  "example.com/users/operator@example.com",
			want: "example.com/users/operator@example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := RedactURLUserinfo(tt.raw); got != tt.want {
				t.Errorf("RedactURLUserinfo(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

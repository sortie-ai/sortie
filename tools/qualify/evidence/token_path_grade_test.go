package evidence

import "testing"

func TestTokenPathGrade(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind string
		want Grade
	}{
		{name: "spend kind grades usable", kind: "spend", want: GradeUsable},
		{name: "occupancy kind grades corroboration-only", kind: "occupancy", want: GradeCorroborationOnly},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := TokenPathGrade(tt.kind); got != tt.want {
				t.Errorf("TokenPathGrade(%q) = %s, want %s", tt.kind, got, tt.want)
			}
		})
	}
}

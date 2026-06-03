package incident

import "testing"

func TestMeetsSeverity(t *testing.T) {
	tests := []struct {
		minimum string
		actual  string
		want    bool
	}{
		{minimum: "", actual: "low", want: true},
		{minimum: "high", actual: "critical", want: true},
		{minimum: "high", actual: "high", want: true},
		{minimum: "high", actual: "medium", want: false},
	}

	for _, tt := range tests {
		if got := meetsSeverity(tt.minimum, tt.actual); got != tt.want {
			t.Fatalf("meetsSeverity(%q, %q) = %v, want %v", tt.minimum, tt.actual, got, tt.want)
		}
	}
}

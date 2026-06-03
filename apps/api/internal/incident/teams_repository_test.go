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

func TestValidateTeamsRouteRequest(t *testing.T) {
	if err := validateTeamsRouteRequest("", "team-1", "channel-1", []CreateTeamsRouteRecipient{{Type: TeamsRecipientTypeUser, TeamsObjectID: "user-1", DisplayName: "Alice"}}); err == nil {
		t.Fatal("expected missing name validation error")
	}
	if err := validateTeamsRouteRequest("route", "team-1", "channel-1", nil); err == nil {
		t.Fatal("expected recipients validation error")
	}
	if err := validateTeamsRouteRequest("route", "team-1", "channel-1", []CreateTeamsRouteRecipient{{Type: "group", TeamsObjectID: "x", DisplayName: "Bad"}}); err == nil {
		t.Fatal("expected recipient type validation error")
	}
	if err := validateTeamsRouteRequest("route", "team-1", "channel-1", []CreateTeamsRouteRecipient{{Type: TeamsRecipientTypeTag, TeamsObjectID: "tag-1", DisplayName: "on-call"}}); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

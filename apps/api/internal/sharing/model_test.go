package sharing

import (
	"testing"
	"time"
)

func TestInvitationDeriveStatus(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	cases := []struct {
		name string
		inv  Invitation
		want string
	}{
		{
			name: "pending: future expiry, no accept/revoke",
			inv:  Invitation{ExpiresAt: future},
			want: "pending",
		},
		{
			name: "expired: past expiry, no accept/revoke",
			inv:  Invitation{ExpiresAt: past},
			want: "expired",
		},
		{
			name: "accepted beats expired",
			inv:  Invitation{ExpiresAt: past, AcceptedAt: &past},
			want: "accepted",
		},
		{
			name: "revoked beats accepted (precedence)",
			inv:  Invitation{ExpiresAt: future, AcceptedAt: &past, RevokedAt: &now},
			want: "revoked",
		},
		{
			name: "revoked beats expired",
			inv:  Invitation{ExpiresAt: past, RevokedAt: &now},
			want: "revoked",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.inv.DeriveStatus(now); got != tc.want {
				t.Fatalf("DeriveStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

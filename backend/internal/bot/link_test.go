package bot

import (
	"testing"

	"launcher-backend/internal/models"
)

func TestLinkStepUpGate(t *testing.T) {
	step := int64(123)
	for name, tc := range map[string]struct {
		user   *models.User
		marker *int64
		want   bool
	}{
		"2fa disabled":         {user: &models.User{}, want: true},
		"2fa enabled no grant": {user: &models.User{TOTPEnabled: true, TOTPSecret: "secret", TOTPLastStep: step}, want: false},
		"matching grant":       {user: &models.User{TOTPEnabled: true, TOTPSecret: "secret", TOTPLastStep: step}, marker: &step, want: true},
		"stale grant":          {user: &models.User{TOTPEnabled: true, TOTPSecret: "secret", TOTPLastStep: step + 1}, marker: &step, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := linkStepUpSatisfied(tc.user, tc.marker); got != tc.want {
				t.Fatalf("linkStepUpSatisfied() = %v, want %v", got, tc.want)
			}
		})
	}
}

package bot

import (
	"testing"

	"launcher-backend/internal/models"
)

func TestRoleRank(t *testing.T) {
	cases := []struct {
		role string
		want int
	}{
		{models.RoleAdmin, 2},
		{models.RoleModerator, 1},
		{models.RoleUser, 0},
		{"", 0},
		{"unknown", 0},
		{"ADMIN", 2},       // регистронезависимо
		{" moderator ", 1}, // с пробелами
	}
	for _, c := range cases {
		if got := roleRank(c.role); got != c.want {
			t.Errorf("roleRank(%q) = %d, want %d", c.role, got, c.want)
		}
	}
}

// TestRolePrivilegeGuardMatrix проверяет правило «нельзя трогать роль не ниже своей».
// Это ядро защиты от эскалации moderator → admin (ensureCanManageTarget использует
// то же сравнение roleRank(target) >= roleRank(actor)).
func TestRolePrivilegeGuardMatrix(t *testing.T) {
	denied := func(actor, target string) bool {
		return roleRank(target) >= roleRank(actor)
	}
	cases := []struct {
		actor, target string
		wantDenied    bool
	}{
		{models.RoleModerator, models.RoleUser, false},     // модератор → игрок: можно
		{models.RoleModerator, models.RoleModerator, true}, // модератор → модератор: нельзя
		{models.RoleModerator, models.RoleAdmin, true},     // модератор → админ: нельзя (эскалация)
		{models.RoleAdmin, models.RoleUser, false},         // админ → игрок: можно
		{models.RoleAdmin, models.RoleModerator, false},    // админ → модератор: можно
		{models.RoleAdmin, models.RoleAdmin, true},         // админ → админ: нельзя
	}
	for _, c := range cases {
		if got := denied(c.actor, c.target); got != c.wantDenied {
			t.Errorf("actor=%s target=%s: denied=%v, want %v", c.actor, c.target, got, c.wantDenied)
		}
	}
}

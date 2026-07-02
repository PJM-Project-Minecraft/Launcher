package auth

import (
	"net/http"
	"strings"

	"launcher-backend/internal/models"

	"github.com/gofiber/fiber/v3"
)

const currentUserKey = "current-user"

func (s Service) RequireAuth() fiber.Handler {
	return func(c fiber.Ctx) error {
		token := bearerToken(c.Get("Authorization"))
		if token == "" {
			return c.Status(http.StatusUnauthorized).JSON(ErrorResponse{Message: "Требуется авторизация"})
		}

		user, err := s.UserFromToken(c.Context(), token)
		if err != nil {
			return c.Status(http.StatusUnauthorized).JSON(ErrorResponse{Message: "Сессия недействительна"})
		}

		// Забаненный аккаунт не должен пользоваться API даже с уже выданным JWT
		// (TTL до 7 дней). Проверяем оба флага бана при каждом запросе.
		if user.IsBanned || user.IsHwidBanned {
			return c.Status(http.StatusForbidden).JSON(ErrorResponse{Message: "Аккаунт заблокирован"})
		}

		c.Locals(currentUserKey, user)
		return c.Next()
	}
}

func RequireAdmin(c fiber.Ctx) error {
	user, ok := CurrentUser(c)
	if !ok {
		return c.Status(http.StatusUnauthorized).JSON(ErrorResponse{Message: "Требуется авторизация"})
	}
	if user.Role != "admin" {
		return c.Status(http.StatusForbidden).JSON(ErrorResponse{Message: "Недостаточно прав"})
	}
	return c.Next()
}

func CurrentUser(c fiber.Ctx) (models.User, bool) {
	user, ok := c.Locals(currentUserKey).(models.User)
	return user, ok
}

func bearerToken(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	value, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		value, ok = strings.CutPrefix(header, "bearer ")
	}
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

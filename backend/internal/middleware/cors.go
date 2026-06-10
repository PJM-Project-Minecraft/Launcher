package middleware

import "github.com/gofiber/fiber/v3"

func CORS(allowedOrigins []string) fiber.Handler {
	return func(c fiber.Ctx) error {
		origin := c.Get("Origin")
		if origin != "" && isAllowedOrigin(origin, allowedOrigins) {
			c.Set("Access-Control-Allow-Origin", origin)
			// Credentials разрешаем только для конкретного совпавшего origin,
			// иначе wildcard в allowlist открывал бы любые сайты к кукам/токенам.
			c.Set("Access-Control-Allow-Credentials", "true")
		}
		c.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		c.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		if c.Method() == fiber.MethodOptions {
			return c.SendStatus(fiber.StatusNoContent)
		}
		return c.Next()
	}
}

func isAllowedOrigin(origin string, allowedOrigins []string) bool {
	// "*" намеренно не поддерживается: API работает с credentials,
	// и wildcard вместе с Allow-Credentials — это дыра (любой сайт получает доступ).
	for _, allowedOrigin := range allowedOrigins {
		if allowedOrigin == origin {
			return true
		}
	}
	return false
}

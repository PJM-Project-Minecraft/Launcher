package middleware

import (
	"io"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v3"
)

// BodyBudget bounds request ingestion before route handlers decode JSON. Prefix
// budgets override the default; exact exempt paths are left to a route-owned
// streaming parser (used for large multipart release uploads).
func BodyBudget(defaultMax int, prefixBudgets map[string]int, exemptPaths map[string]struct{}) fiber.Handler {
	return func(c fiber.Ctx) error {
		path := c.Path()
		if _, exempt := exemptPaths[path]; exempt {
			return c.Next()
		}
		maxBytes := defaultMax
		for prefix, budget := range prefixBudgets {
			if strings.HasPrefix(path, prefix) {
				maxBytes = budget
				break
			}
		}
		if maxBytes <= 0 {
			return c.Next()
		}
		if contentLength := c.RequestCtx().Request.Header.ContentLength(); contentLength > maxBytes {
			return c.SendStatus(http.StatusRequestEntityTooLarge)
		}

		request := &c.RequestCtx().Request
		if !request.IsBodyStream() {
			if len(request.Body()) > maxBytes {
				return c.SendStatus(http.StatusRequestEntityTooLarge)
			}
			return c.Next()
		}

		body, err := io.ReadAll(io.LimitReader(request.BodyStream(), int64(maxBytes)+1))
		_ = request.CloseBodyStream()
		if err != nil {
			return c.SendStatus(http.StatusBadRequest)
		}
		if len(body) > maxBytes {
			return c.SendStatus(http.StatusRequestEntityTooLarge)
		}
		request.SetBodyRaw(body)
		return c.Next()
	}
}

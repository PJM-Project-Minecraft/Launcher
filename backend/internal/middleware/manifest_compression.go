package middleware

import (
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/compress"
)

// ManifestCompression negotiates fast gzip/brotli/zstd compression only for
// player profile manifests. They contain thousands of repetitive JSON records,
// while bundles and objects are already compressed or need byte-range semantics.
func ManifestCompression() fiber.Handler {
	return compress.New(compress.Config{
		Level: compress.LevelBestSpeed,
		Next: func(c fiber.Ctx) bool {
			path := c.Path()
			return !strings.HasPrefix(path, "/api/profiles/") ||
				!strings.HasSuffix(path, "/manifest")
		},
	})
}

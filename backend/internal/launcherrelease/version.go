// Package launcherrelease — релизы десктоп-лаунчера: хранение бинарников,
// проверка обновлений и вычисление обязательной минимальной версии.
package launcherrelease

import (
	"strconv"
	"strings"
)

// CompareVersions сравнивает версии "X.Y.Z" посегментно: -1 если a < b,
// 0 если равны, 1 если a > b. Отсутствующие или нечисловые сегменты
// считаются нулями ("1.2" == "1.2.0"; мусор в заголовке = "0.0.0").
func CompareVersions(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		av, bv := segment(as, i), segment(bs, i)
		switch {
		case av < bv:
			return -1
		case av > bv:
			return 1
		}
	}
	return 0
}

func segment(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	n, err := strconv.Atoi(parts[i])
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// ValidVersion — строгий формат X.Y.Z (только цифры) для входных данных админки.
func ValidVersion(v string) bool {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

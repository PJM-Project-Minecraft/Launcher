//go:build !linux

package profiles

import "os"

// На платформе без доступного inode change-time не доверяем metadata-кэшу:
// Scan пересчитает SHA и сохранит прежнюю модель целостности.
func sourceChangeTimeNS(_ os.FileInfo) int64 {
	return 0
}

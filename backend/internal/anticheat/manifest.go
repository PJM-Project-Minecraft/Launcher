package anticheat

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"time"
)

// ArtifactManifest — SHA-256 артефактов, которые лаунчер инжектит в JVM. Клиент
// сверяет ими скачанные agent.jar / нативную библиотеку / authlib-injector ПЕРЕД
// инжектом: несовпадение = подмена (MITM или локально) → блок запуска. Закрывает
// вектор «подменённый -javaagent/-agentpath → произвольный код в JVM».
type ArtifactManifest struct {
	AgentSha256   string       `json:"agentSha256,omitempty"`
	AuthlibSha256 string       `json:"authlibSha256,omitempty"`
	Native        NativeHashes `json:"native"`
}

type NativeHashes struct {
	Linux   string `json:"linux,omitempty"`
	Windows string `json:"windows,omitempty"`
}

// shaEntry — кэш-запись: хэш валиден, пока не изменились mtime и размер файла.
type shaEntry struct {
	modTime time.Time
	size    int64
	sha     string
}

// Manifest возвращает SHA-256 артефактов. Хэш пересчитывается только при изменении
// файла (кэш по mtime+size), отсутствующий файл даёт пустую строку (поле опускается).
func (s *Service) Manifest() ArtifactManifest {
	return ArtifactManifest{
		AgentSha256:   s.cachedSha(s.agentPath),
		AuthlibSha256: s.cachedSha(s.authlibPath),
		Native: NativeHashes{
			Linux:   s.cachedSha(s.nativeLinux),
			Windows: s.cachedSha(s.nativeWin),
		},
	}
}

func (s *Service) cachedSha(path string) string {
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	s.shaMu.Lock()
	defer s.shaMu.Unlock()
	if e, ok := s.shaEntries[path]; ok && e.modTime.Equal(info.ModTime()) && e.size == info.Size() {
		return e.sha
	}
	sha := fileSha256(path)
	if sha != "" {
		s.shaEntries[path] = shaEntry{modTime: info.ModTime(), size: info.Size(), sha: sha}
	}
	return sha
}

func fileSha256(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

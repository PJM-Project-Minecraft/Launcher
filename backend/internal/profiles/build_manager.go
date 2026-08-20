package profiles

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"sync"
	"time"

	"launcher-backend/internal/models"

	"github.com/google/uuid"
)

const (
	BuildQueued    = "queued"
	BuildRunning   = "running"
	BuildSucceeded = "succeeded"
	BuildFailed    = "failed"
	buildLogLimit  = 250
)

// BuildLog — строка журнала, которую одинаково видят slog и WEB.
type BuildLog struct {
	At      time.Time `json:"at"`
	Level   string    `json:"level"`
	Phase   string    `json:"phase"`
	Message string    `json:"message"`
}

// BuildSnapshot — полное, самодостаточное состояние одной публикации.
// WebSocket может терять промежуточные сообщения: следующий snapshot всё равно
// восстанавливает актуальный экран без replay-протокола.
type BuildSnapshot struct {
	ID        string      `json:"id"`
	ProfileID string      `json:"profileId"`
	Status    string      `json:"status"`
	Phase     string      `json:"phase"`
	Message   string      `json:"message"`
	Progress  float64     `json:"progress"`
	Current   int64       `json:"current"`
	Total     int64       `json:"total"`
	Result    *ScanResult `json:"result,omitempty"`
	Error     string      `json:"error,omitempty"`
	Logs      []BuildLog  `json:"logs"`
	CreatedAt time.Time   `json:"createdAt"`
	StartedAt *time.Time  `json:"startedAt,omitempty"`
	EndedAt   *time.Time  `json:"endedAt,omitempty"`
}

// BuildManager скрывает очередь, жизненный цикл, журнал и fan-out событий за
// небольшим Interface: Start, Snapshot и Subscribe.
type BuildManager struct {
	service     Service
	onPublished func()
	worker      chan struct{}

	mu      sync.Mutex
	jobs    map[string]*BuildSnapshot
	subs    map[int]chan BuildSnapshot
	nextSub int
}

func NewBuildManager(service Service, onPublished func()) *BuildManager {
	return &BuildManager{
		service: service, onPublished: onPublished,
		worker: make(chan struct{}, 1), jobs: make(map[string]*BuildSnapshot),
		subs: make(map[int]chan BuildSnapshot),
	}
}

// Start ставит публикацию в очередь. Повторный вызов для уже активного профиля
// идемпотентен и возвращает существующее задание.
func (m *BuildManager) Start(ctx context.Context, profileID string) (BuildSnapshot, bool, error) {
	var profile models.Profile
	if err := m.service.db.WithContext(ctx).Select("id").First(&profile, "id = ?", profileID).Error; err != nil {
		return BuildSnapshot{}, false, err
	}

	m.mu.Lock()
	if current, ok := m.jobs[profileID]; ok && isBuildActive(current.Status) {
		snapshot := cloneBuild(*current)
		m.mu.Unlock()
		return snapshot, false, nil
	}
	now := time.Now().UTC()
	job := &BuildSnapshot{
		ID: uuid.NewString(), ProfileID: profileID, Status: BuildQueued,
		Phase: "waiting", Message: "Публикация поставлена в очередь",
		Logs:      []BuildLog{{At: now, Level: "info", Phase: "waiting", Message: "Публикация поставлена в очередь"}},
		CreatedAt: now,
	}
	m.jobs[profileID] = job
	snapshot := cloneBuild(*job)
	m.broadcastLocked(snapshot)
	m.mu.Unlock()

	slog.Info("manifest build queued", "job", job.ID, "profile", profileID)
	go m.run(profileID, job.ID)
	return snapshot, true, nil
}

func (m *BuildManager) run(profileID, jobID string) {
	m.worker <- struct{}{}
	defer func() { <-m.worker }()

	now := time.Now().UTC()
	m.update(profileID, jobID, func(job *BuildSnapshot) {
		job.Status = BuildRunning
		job.StartedAt = &now
		job.Message = "Публикация запущена"
		appendBuildLog(job, "info", "waiting", job.Message)
	})
	slog.Info("manifest build started", "job", jobID, "profile", profileID)

	result, err := m.service.ScanWithProgress(context.Background(), profileID, func(progress ScanProgress) {
		m.update(profileID, jobID, func(job *BuildSnapshot) {
			phaseChanged := job.Phase != progress.Phase
			job.Phase = progress.Phase
			job.Message = progress.Message
			job.Progress = clampProgress(progress.Percent)
			job.Current = progress.Current
			job.Total = progress.Total
			if progress.Log || phaseChanged {
				appendBuildLog(job, "info", progress.Phase, progress.Message)
				slog.Info("manifest build progress", "job", jobID, "profile", profileID,
					"phase", progress.Phase, "message", progress.Message)
			}
		})
	})

	ended := time.Now().UTC()
	if err != nil {
		m.update(profileID, jobID, func(job *BuildSnapshot) {
			job.Status = BuildFailed
			job.Error = err.Error()
			job.Message = "Публикация завершилась с ошибкой"
			job.EndedAt = &ended
			appendBuildLog(job, "error", job.Phase, err.Error())
		})
		slog.Error("manifest build failed", "job", jobID, "profile", profileID, "error", err)
		return
	}

	m.update(profileID, jobID, func(job *BuildSnapshot) {
		job.Status = BuildSucceeded
		job.Phase = "done"
		job.Message = "Manifest и bundle опубликованы"
		job.Progress = 1
		job.Result = &result
		job.EndedAt = &ended
		appendBuildLog(job, "info", "done", job.Message)
	})
	slog.Info("manifest build succeeded", "job", jobID, "profile", profileID,
		"files", result.FileCount, "bytes", result.TotalSize)
	if m.onPublished != nil {
		m.onPublished()
	}
}

func (m *BuildManager) update(profileID, jobID string, mutate func(*BuildSnapshot)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[profileID]
	if !ok || job.ID != jobID {
		return
	}
	mutate(job)
	m.broadcastLocked(cloneBuild(*job))
}

func (m *BuildManager) Snapshot(profileID string) (BuildSnapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[profileID]
	if !ok {
		return BuildSnapshot{}, false
	}
	return cloneBuild(*job), true
}

func (m *BuildManager) Snapshots() []BuildSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]BuildSnapshot, 0, len(m.jobs))
	for _, job := range m.jobs {
		result = append(result, cloneBuild(*job))
	}
	return result
}

func (m *BuildManager) Subscribe() (int, <-chan BuildSnapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextSub
	m.nextSub++
	ch := make(chan BuildSnapshot, 16)
	m.subs[id] = ch
	return id, ch
}

func (m *BuildManager) Unsubscribe(id int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ch, ok := m.subs[id]; ok {
		delete(m.subs, id)
		close(ch)
	}
}

func (m *BuildManager) broadcastLocked(snapshot BuildSnapshot) {
	for _, ch := range m.subs {
		select {
		case ch <- snapshot:
		default:
		}
	}
}

func appendBuildLog(job *BuildSnapshot, level, phase, message string) {
	if message == "" {
		return
	}
	job.Logs = append(job.Logs, BuildLog{
		At: time.Now().UTC(), Level: level, Phase: phase, Message: message,
	})
	if len(job.Logs) > buildLogLimit {
		job.Logs = append([]BuildLog(nil), job.Logs[len(job.Logs)-buildLogLimit:]...)
	}
}

func cloneBuild(source BuildSnapshot) BuildSnapshot {
	result := source
	result.Logs = append([]BuildLog(nil), source.Logs...)
	if source.Result != nil {
		copyResult := *source.Result
		result.Result = &copyResult
	}
	return result
}

func isBuildActive(status string) bool {
	return status == BuildQueued || status == BuildRunning
}

func clampProgress(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

// SocketTickets выдаёт одноразовые короткоживущие билеты: JWT не попадает в
// query WebSocket URL и, соответственно, в access-логи reverse proxy.
type SocketTickets struct {
	mu      sync.Mutex
	ttl     time.Duration
	tickets map[string]time.Time
}

func NewSocketTickets(ttl time.Duration) *SocketTickets {
	return &SocketTickets{ttl: ttl, tickets: make(map[string]time.Time)}
}

func (s *SocketTickets) Issue() (string, time.Time, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", time.Time{}, err
	}
	ticket := base64.RawURLEncoding.EncodeToString(bytes)
	expires := time.Now().UTC().Add(s.ttl)
	s.mu.Lock()
	s.deleteExpiredLocked(time.Now().UTC())
	s.tickets[ticket] = expires
	s.mu.Unlock()
	return ticket, expires, nil
}

func (s *SocketTickets) Consume(ticket string) error {
	if ticket == "" {
		return errors.New("missing websocket ticket")
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, ok := s.tickets[ticket]
	delete(s.tickets, ticket)
	if !ok || !expires.After(now) {
		return errors.New("invalid or expired websocket ticket")
	}
	return nil
}

func (s *SocketTickets) deleteExpiredLocked(now time.Time) {
	for ticket, expires := range s.tickets {
		if !expires.After(now) {
			delete(s.tickets, ticket)
		}
	}
}

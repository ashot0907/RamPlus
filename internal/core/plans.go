package core

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// Ошибки Apply по ID плана.
var (
	ErrPlanStale    = errors.New("система изменилась с момента построения плана, постройте план заново")
	ErrPlanNotFound = errors.New("план не найден (не строился или истёк) — постройте план заново")
	ErrPlanUsed     = errors.New("план уже выполнялся — постройте план заново")
)

// Сколько и как долго хранятся построенные планы.
const (
	maxStoredPlans = 64
	planTTL        = 15 * time.Minute
)

type storedPlan struct {
	req         Request
	fingerprint string
	at          time.Time
	used        bool // Apply уже забрал план (запись остаётся до истечения — для ErrPlanUsed)
}

// planStore — построенные, но ещё не выполненные планы (для демона: plan
// и apply — разные запросы). Безопасен для параллельного использования.
type planStore struct {
	mu    sync.Mutex
	plans map[string]storedPlan
	order []string // в порядке построения, для вытеснения старых
}

func (s *planStore) put(r Request, fingerprint string, now time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.plans == nil {
		s.plans = map[string]storedPlan{}
	}
	s.expire(now)
	for len(s.order) >= maxStoredPlans {
		delete(s.plans, s.order[0])
		s.order = s.order[1:]
	}
	id := newPlanID()
	s.plans[id] = storedPlan{req: r, fingerprint: fingerprint, at: now}
	s.order = append(s.order, id)
	return id
}

// take забирает план: выполнить его можно только один раз.
func (s *planStore) take(id string, now time.Time) (storedPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire(now)
	p, ok := s.plans[id]
	switch {
	case !ok:
		return p, ErrPlanNotFound
	case p.used:
		return p, ErrPlanUsed
	}
	p.used = true
	s.plans[id] = p
	return p, nil
}

func (s *planStore) expire(now time.Time) {
	for len(s.order) > 0 {
		p, ok := s.plans[s.order[0]]
		if ok && now.Sub(p.at) <= planTTL {
			return
		}
		delete(s.plans, s.order[0])
		s.order = s.order[1:]
	}
}

func newPlanID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

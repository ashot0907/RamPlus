package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"sync"
	"time"
)

// Параметры сессий и ограничения попыток входа.
const (
	SessionTTL      = 12 * time.Hour
	maxSessions     = 64
	FailWindow      = 5 * time.Minute // окно подсчёта неудачных попыток
	FailLimit       = 5               // неудачных попыток в окне до паузы
	firstLockout    = 30 * time.Second
	maxLockout      = 15 * time.Minute
	limiterForgetIn = time.Hour // после часа без ошибок счётчик пауз сбрасывается
)

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand в Linux не возвращает ошибок
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// session — вход по паролю (в памяти ramplus-web).
type session struct {
	csrf        string
	expires     time.Time
	authVersion string // версия файла пароля: passwd завершает сессии
}

type sessionStore struct {
	mu   sync.Mutex
	m    map[string]*session
	list []string // порядок создания (вытеснение старых)
}

func newSessionStore() *sessionStore { return &sessionStore{m: map[string]*session{}} }

func (s *sessionStore) create(now time.Time, ttl time.Duration, authVersion string) (string, *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(now)
	for len(s.list) >= maxSessions {
		delete(s.m, s.list[0])
		s.list = s.list[1:]
	}
	id := randomToken()
	ss := &session{csrf: randomToken(), expires: now.Add(ttl), authVersion: authVersion}
	s.m[id] = ss
	s.list = append(s.list, id)
	return id, ss
}

// get — действующая сессия (истёкшая или от старого пароля удаляется).
func (s *sessionStore) get(id string, now time.Time, authVersion string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.m[id]
	if !ok {
		return nil
	}
	if !now.Before(ss.expires) || ss.authVersion != authVersion {
		s.deleteLocked(id)
		return nil
	}
	return ss
}

func (s *sessionStore) delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteLocked(id)
}

func (s *sessionStore) deleteLocked(id string) {
	delete(s.m, id)
	for i, v := range s.list {
		if v == id {
			s.list = append(s.list[:i], s.list[i+1:]...)
			break
		}
	}
}

func (s *sessionStore) expireLocked(now time.Time) {
	for _, id := range append([]string(nil), s.list...) {
		if !now.Before(s.m[id].expires) {
			s.deleteLocked(id)
		}
	}
}

func (s *sessionStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

func csrfOK(ss *session, token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(ss.csrf), []byte(token)) == 1
}

// limiter — неудачные входы по адресу клиента: FailLimit за FailWindow →
// пауза 30 с, каждая следующая пауза вдвое длиннее (до 15 мин).
type limiter struct {
	mu sync.Mutex
	m  map[string]*attempts
}

type attempts struct {
	fails   []time.Time
	until   time.Time
	strikes int // сколько раз уже была пауза
	last    time.Time
}

func newLimiter() *limiter { return &limiter{m: map[string]*attempts{}} }

// blocked — сколько ещё ждать (0 — можно пробовать).
func (l *limiter) blocked(ip string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.m[ip]
	if !ok || !now.Before(a.until) {
		return 0
	}
	return a.until.Sub(now)
}

// fail учитывает неудачную попытку; возвращает паузу, если она началась,
// и сколько попыток осталось до паузы.
func (l *limiter) fail(ip string, now time.Time) (lock time.Duration, left int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.m[ip]
	if !ok || now.Sub(a.last) > limiterForgetIn {
		a = &attempts{}
		l.m[ip] = a
	}
	a.last = now
	kept := a.fails[:0]
	for _, t := range a.fails {
		if now.Sub(t) < FailWindow {
			kept = append(kept, t)
		}
	}
	a.fails = append(kept, now)
	if len(a.fails) < FailLimit {
		return 0, FailLimit - len(a.fails)
	}
	lock = firstLockout << min(a.strikes, 10)
	lock = min(lock, maxLockout)
	a.strikes++
	a.until = now.Add(lock)
	a.fails = a.fails[:0]
	return lock, 0
}

func (l *limiter) success(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, ip)
}

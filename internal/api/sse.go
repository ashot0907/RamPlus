package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"github.com/ashot0907/ramplus/internal/monitor"
)

// sseWriter — поток Server-Sent Events. Буфер и кодировщик переиспользуются
// между событиями (без лишних аллокаций в цикле).
type sseWriter struct {
	w   http.ResponseWriter
	rc  *http.ResponseController
	buf bytes.Buffer
	enc *json.Encoder
}

func startSSE(w http.ResponseWriter) *sseWriter {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	e := &sseWriter{w: w, rc: http.NewResponseController(w)}
	e.enc = json.NewEncoder(&e.buf)
	return e
}

// send пишет событие name с JSON-данными v (одной строкой).
func (e *sseWriter) send(name string, v any) error {
	e.buf.Reset()
	e.buf.WriteString("event: ")
	e.buf.WriteString(name)
	e.buf.WriteString("\ndata: ")
	if err := e.enc.Encode(v); err != nil { // Encode завершает строку \n
		return err
	}
	e.buf.WriteByte('\n')
	return e.flush()
}

// ping — комментарий SSE: держит соединение и выявляет ушедших клиентов.
func (e *sseWriter) ping() error {
	e.buf.Reset()
	e.buf.WriteString(": ping\n\n")
	return e.flush()
}

func (e *sseWriter) flush() error {
	_ = e.rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	if _, err := e.w.Write(e.buf.Bytes()); err != nil {
		return err
	}
	return e.rc.Flush()
}

// StatusFrame — событие status в /v1/status/stream.
type StatusFrame struct {
	Status monitor.Status `json:"status"`
	Peaks  *monitor.Peaks `json:"peaks"`
}

// statusStream — GET /v1/status/stream: снимок раз в StatusInterval и
// пики за время наблюдения. Клиент отключился или демон останавливается —
// горутина и таймер освобождаются.
func (s *Server) statusStream(w http.ResponseWriter, r *http.Request) {
	s.streams.Add(1)
	defer s.streams.Add(-1)
	e := startSSE(w)
	t := time.NewTicker(s.StatusInterval)
	defer t.Stop()
	frame := StatusFrame{Peaks: monitor.NewPeaks(time.Now())}
	for {
		now := time.Now()
		frame.Status = s.Core.Status()
		frame.Peaks.Update(frame.Status, now)
		if err := e.send("status", &frame); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
		}
	}
}

// operationEvents — GET /v1/operations/{id}/events: уже выполненные шаги,
// затем новые по мере выполнения (event: step), в конце итог (event:
// result) и закрытие потока.
func (s *Server) operationEvents(w http.ResponseWriter, r *http.Request) {
	op, ok := s.lookupOp(w, r)
	if !ok {
		return
	}
	s.streams.Add(1)
	defer s.streams.Add(-1)
	e := startSSE(w)
	hb := time.NewTicker(sseHeartbeat)
	defer hb.Stop()
	for from := 0; ; {
		evs, res, changed := op.Watch(from)
		for i := range evs {
			if err := e.send("step", &evs[i]); err != nil {
				return
			}
		}
		from += len(evs)
		if res != nil {
			_ = e.send("result", res)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-changed:
		case <-hb.C:
			if err := e.ping(); err != nil {
				return
			}
		}
	}
}

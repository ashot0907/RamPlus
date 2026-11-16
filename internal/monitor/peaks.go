package monitor

import "time"

// Peaks — максимумы за время наблюдения (status --watch).
type Peaks struct {
	Since   time.Time `json:"since"`
	Samples int       `json:"samples"`

	SwapUsedBytes  uint64    `json:"swap_used_bytes"`
	SwapUsedAt     time.Time `json:"swap_used_at,omitzero"`
	ZswapPoolBytes uint64    `json:"zswap_pool_bytes"`
	PSISomeAvg10   float64   `json:"psi_some_avg10"`
	PSISomeAt      time.Time `json:"psi_some_at,omitzero"`
	PSIFullAvg10   float64   `json:"psi_full_avg10"`
	PSIFullAt      time.Time `json:"psi_full_at,omitzero"`
	MemUsedBytes   uint64    `json:"mem_used_bytes"`
	PressureLevel  string    `json:"pressure_level"` // по пику some avg10
}

// NewPeaks начинает наблюдение.
func NewPeaks(now time.Time) *Peaks { return &Peaks{Since: now} }

// Update учитывает очередной снимок.
func (p *Peaks) Update(st Status, now time.Time) {
	p.Samples++
	if v := st.Memory.SwapUsedBytes; v != nil && *v > p.SwapUsedBytes {
		p.SwapUsedBytes, p.SwapUsedAt = *v, now
	}
	if v := st.Memory.UsedBytes; v != nil && *v > p.MemUsedBytes {
		p.MemUsedBytes = *v
	}
	if v := st.Zswap.PoolBytes; v != nil && *v > p.ZswapPoolBytes {
		p.ZswapPoolBytes = *v
	}
	if s := st.PSI.Some; s != nil && (s.Avg10 > p.PSISomeAvg10 || p.PSISomeAt.IsZero()) {
		p.PSISomeAvg10, p.PSISomeAt = s.Avg10, now
	}
	if f := st.PSI.Full; f != nil && (f.Avg10 > p.PSIFullAvg10 || p.PSIFullAt.IsZero()) {
		p.PSIFullAvg10, p.PSIFullAt = f.Avg10, now
	}
	if !p.PSISomeAt.IsZero() {
		p.PressureLevel = PressureLevelFor(p.PSISomeAvg10)
	}
}

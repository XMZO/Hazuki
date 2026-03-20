package adaptmodel

import "sort"

type P2Quantile struct {
	q       float64
	count   int64
	ready   bool
	initial [5]float64
	h       [5]float64
	n       [5]int
	np      [5]float64
	dn      [5]float64
}

type LinearRLSSnapshot struct {
	Intercept float64
	Slope     float64
	Samples   int64
}

type LinearRLS struct {
	lambda  float64
	samples int64
	beta0   float64
	beta1   float64
	p00     float64
	p01     float64
	p10     float64
	p11     float64
}

func NewP2Quantile(q float64) P2Quantile {
	if q <= 0 {
		q = 0.5
	}
	if q >= 1 {
		q = 0.999
	}
	return P2Quantile{q: q}
}

func (p *P2Quantile) Observe(x float64) {
	if p == nil {
		return
	}
	if !p.ready {
		if p.count < int64(len(p.initial)) {
			p.initial[p.count] = x
			p.count++
			if p.count == int64(len(p.initial)) {
				p.bootstrap()
			}
			return
		}
	}

	k := 0
	switch {
	case x < p.h[0]:
		p.h[0] = x
		k = 0
	case x >= p.h[4]:
		p.h[4] = x
		k = 3
	default:
		for i := 0; i < 4; i++ {
			if x < p.h[i+1] {
				k = i
				break
			}
		}
	}

	for i := k + 1; i < 5; i++ {
		p.n[i]++
	}
	for i := 0; i < 5; i++ {
		p.np[i] += p.dn[i]
	}
	for i := 1; i <= 3; i++ {
		d := p.np[i] - float64(p.n[i])
		if (d >= 1 && p.n[i+1]-p.n[i] > 1) || (d <= -1 && p.n[i-1]-p.n[i] < -1) {
			sign := 1
			if d < 0 {
				sign = -1
			}
			qhat := p.parabolic(i, sign)
			if qhat > p.h[i-1] && qhat < p.h[i+1] {
				p.h[i] = qhat
			} else {
				p.h[i] = p.linear(i, sign)
			}
			p.n[i] += sign
		}
	}
	p.count++
}

func (p *P2Quantile) Estimate(fallback float64) float64 {
	if p == nil {
		return fallback
	}
	if !p.ready {
		if p.count == 0 {
			return fallback
		}
		samples := make([]float64, p.count)
		copy(samples, p.initial[:p.count])
		sort.Float64s(samples)
		idx := int(float64(len(samples)-1) * p.q)
		if idx < 0 {
			idx = 0
		}
		if idx >= len(samples) {
			idx = len(samples) - 1
		}
		return samples[idx]
	}
	return p.h[2]
}

func (p *P2Quantile) Count() int64 {
	if p == nil {
		return 0
	}
	return p.count
}

func (p *P2Quantile) bootstrap() {
	samples := make([]float64, len(p.initial))
	copy(samples, p.initial[:])
	sort.Float64s(samples)
	copy(p.h[:], samples)
	for i := 0; i < 5; i++ {
		p.n[i] = i + 1
	}
	p.np[0] = 1
	p.np[1] = 1 + 2*p.q
	p.np[2] = 1 + 4*p.q
	p.np[3] = 3 + 2*p.q
	p.np[4] = 5
	p.dn[0] = 0
	p.dn[1] = p.q / 2
	p.dn[2] = p.q
	p.dn[3] = (1 + p.q) / 2
	p.dn[4] = 1
	p.ready = true
}

func (p *P2Quantile) parabolic(i, sign int) float64 {
	n0 := float64(p.n[i-1])
	n1 := float64(p.n[i])
	n2 := float64(p.n[i+1])
	q0 := p.h[i-1]
	q1 := p.h[i]
	q2 := p.h[i+1]
	ds := float64(sign)
	left := (n1 - n0 + ds) * (q2 - q1) / (n2 - n1)
	right := (n2 - n1 - ds) * (q1 - q0) / (n1 - n0)
	return q1 + ds*(left+right)/(n2-n0)
}

func (p *P2Quantile) linear(i, sign int) float64 {
	j := i + sign
	return p.h[i] + float64(sign)*(p.h[j]-p.h[i])/float64(p.n[j]-p.n[i])
}

func NewLinearRLS(defaultIntercept, defaultSlope, lambda, initialVariance float64) LinearRLS {
	if lambda <= 0 || lambda > 1 {
		lambda = 0.98
	}
	if initialVariance <= 0 {
		initialVariance = 64
	}
	return LinearRLS{
		lambda: lambda,
		beta0:  defaultIntercept,
		beta1:  defaultSlope,
		p00:    initialVariance,
		p11:    initialVariance,
	}
}

func (m *LinearRLS) Observe(x, y float64) {
	if m == nil {
		return
	}
	denom := m.lambda + m.p00 + x*(m.p10+m.p01) + x*x*m.p11
	if denom <= 0 {
		return
	}

	k0 := (m.p00 + m.p01*x) / denom
	k1 := (m.p10 + m.p11*x) / denom
	pred := m.beta0 + m.beta1*x
	err := y - pred

	m.beta0 += k0 * err
	m.beta1 += k1 * err

	p00 := (m.p00 - k0*(m.p00+m.p10*x)) / m.lambda
	p01 := (m.p01 - k0*(m.p01+m.p11*x)) / m.lambda
	p10 := (m.p10 - k1*(m.p00+m.p10*x)) / m.lambda
	p11 := (m.p11 - k1*(m.p01+m.p11*x)) / m.lambda

	m.p00 = p00
	m.p01 = p01
	m.p10 = p10
	m.p11 = p11
	m.samples++
}

func (m *LinearRLS) Predict(x float64) float64 {
	if m == nil {
		return 0
	}
	return m.beta0 + m.beta1*x
}

func (m *LinearRLS) Snapshot() LinearRLSSnapshot {
	if m == nil {
		return LinearRLSSnapshot{}
	}
	return LinearRLSSnapshot{
		Intercept: m.beta0,
		Slope:     m.beta1,
		Samples:   m.samples,
	}
}

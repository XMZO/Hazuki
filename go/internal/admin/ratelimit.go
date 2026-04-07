package admin

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	loginMaxAttempts = 5
	loginWindow      = 15 * time.Minute
	loginCleanup     = 5 * time.Minute
)

type loginLimiter struct {
	mu      sync.Mutex
	buckets map[string]*attemptBucket
}

type attemptBucket struct {
	timestamps []time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{buckets: make(map[string]*attemptBucket)}
}

// startCleanup removes stale buckets periodically. Call from a goroutine.
func (l *loginLimiter) startCleanup(stop <-chan struct{}) {
	ticker := time.NewTicker(loginCleanup)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-ticker.C:
			l.mu.Lock()
			cutoff := now.Add(-loginWindow)
			for ip, b := range l.buckets {
				b.timestamps = pruneOld(b.timestamps, cutoff)
				if len(b.timestamps) == 0 {
					delete(l.buckets, ip)
				}
			}
			l.mu.Unlock()
		}
	}
}

// check returns true if the IP is allowed to attempt login.
func (l *loginLimiter) check(ip string) bool {
	if ip == "" {
		return true
	}
	now := time.Now()
	cutoff := now.Add(-loginWindow)

	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.buckets[ip]
	if b == nil {
		return true
	}
	b.timestamps = pruneOld(b.timestamps, cutoff)
	return len(b.timestamps) < loginMaxAttempts
}

// recordFailure records a failed login attempt for the given IP.
func (l *loginLimiter) recordFailure(ip string) {
	if ip == "" {
		return
	}
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.buckets[ip]
	if b == nil {
		b = &attemptBucket{}
		l.buckets[ip] = b
	}
	b.timestamps = append(b.timestamps, now)
}

// clearIP removes all records for the given IP (call on successful login).
func (l *loginLimiter) clearIP(ip string) {
	if ip == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, ip)
}

// extractClientIP returns the best guess at the real client IP.
func extractClientIP(r *http.Request) string {
	// Trust Cf-Connecting-Ip first (Cloudflare).
	if ip := normalizeAdminIP(r.Header.Get("Cf-Connecting-Ip")); ip != "" {
		return ip
	}
	// Then X-Forwarded-For (first entry = original client).
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		first := strings.TrimSpace(strings.Split(xff, ",")[0])
		if ip := normalizeAdminIP(first); ip != "" {
			return ip
		}
	}
	// Then X-Real-IP (single-proxy setups).
	if ip := normalizeAdminIP(r.Header.Get("X-Real-Ip")); ip != "" {
		return ip
	}
	// Fallback to RemoteAddr.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func pruneOld(ts []time.Time, cutoff time.Time) []time.Time {
	n := 0
	for _, t := range ts {
		if t.After(cutoff) {
			ts[n] = t
			n++
		}
	}
	return ts[:n]
}

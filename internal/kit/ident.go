package kit

import (
	"crypto/rand"
	"time"
)

func WithRetryJitter(delay time.Duration) time.Duration {
	if delay <= 0 {
		return delay
	}
	// 在退避时间上增加 0%~25% 抖动，避免多个请求同时重试。
	jitter := time.Duration(float64(delay) * float64(RandIntn(26)) / 100)
	const maxDuration = time.Duration(1<<63 - 1)
	if delay > maxDuration-jitter {
		return maxDuration
	}
	return delay + jitter
}

func RandIntn(n int) int {
	if n <= 0 {
		return 0
	}
	b := make([]byte, 4)
	rand.Read(b)
	v := int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	if v < 0 {
		v = -v
	}
	return v % n
}

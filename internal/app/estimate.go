package app

import "encoding/json"

// estimateJSON provides a cheap token estimate for local usage accounting.
func estimateJSON(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b) / 4
}

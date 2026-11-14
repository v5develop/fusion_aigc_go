package core

import (
	"database/sql"
	"encoding/json"
	"sort"
)

func sortSlice[T any](s []T, less func(a, b T) bool) {
	sort.SliceStable(s, func(i, j int) bool { return less(s[i], s[j]) })
}

func sqlNullTime() sql.NullTime { return sql.NullTime{} }

// jsonMarshal2 结构体 → map（审计快照用）。
func jsonMarshal2(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	err = json.Unmarshal(b, &m)
	return m, err
}

package db

func mapStr(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func mapInt64(m map[string]any, key string) int64 {
	v, _ := m[key].(int64)
	return v
}

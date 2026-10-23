package session

// Value returns the value of key as a T, converting through JSON as
// Decode does, so values of sessions loaded from disk, which are plain
// JSON values, read as the type they were stored as. It reports false if
// s has no such value.
func (s *State) Value[T any](key string) (T, bool, error) {
	var v T
	ok, err := s.Decode(key, &v)
	return v, ok, err
}

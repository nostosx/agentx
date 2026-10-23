//go:build windows && !(386 || arm)

package process

// jobBasicLimitsPad is empty: on 64-bit systems Go aligns
// JOBOBJECT_BASIC_LIMIT_INFORMATION like C.
type jobBasicLimitsPad [0]uint32

const (
	jobBasicLimitsSize    = 64
	jobExtendedLimitsSize = 144
)

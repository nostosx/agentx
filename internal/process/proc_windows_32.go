//go:build windows && (386 || arm)

package process

// jobBasicLimitsPad pads JOBOBJECT_BASIC_LIMIT_INFORMATION from the 44
// bytes Go lays out to the 48 of its 8-byte C alignment.
type jobBasicLimitsPad [1]uint32

const (
	jobBasicLimitsSize    = 48
	jobExtendedLimitsSize = 112
)

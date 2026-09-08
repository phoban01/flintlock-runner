package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ByteSize is a byte quantity written in YAML as a human-readable string
// such as "50GiB", "512MB" or "1073741824". Binary units (KiB, MiB, GiB,
// TiB) are powers of 1024; decimal units (KB, MB, GB, TB) are powers of 1000.
// It implements encoding.TextUnmarshaler so that gopkg.in/yaml.v3 decodes it
// without this package importing yaml.
type ByteSize int64

// ErrInvalidByteSize is wrapped by UnmarshalText for malformed input.
var ErrInvalidByteSize = errors.New("config: invalid byte size")

var byteUnits = map[string]int64{
	"":    1,
	"b":   1,
	"kb":  1000,
	"mb":  1000 * 1000,
	"gb":  1000 * 1000 * 1000,
	"tb":  1000 * 1000 * 1000 * 1000,
	"kib": 1 << 10,
	"mib": 1 << 20,
	"gib": 1 << 30,
	"tib": 1 << 40,
}

// ParseByteSize parses s into a ByteSize.
func ParseByteSize(s string) (ByteSize, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, fmt.Errorf("%w: empty", ErrInvalidByteSize)
	}
	i := 0
	for i < len(trimmed) && (trimmed[i] >= '0' && trimmed[i] <= '9' || trimmed[i] == '.') {
		i++
	}
	num, unit := trimmed[:i], strings.ToLower(strings.TrimSpace(trimmed[i:]))
	mult, ok := byteUnits[unit]
	if !ok {
		return 0, fmt.Errorf("%w: unknown unit %q in %q", ErrInvalidByteSize, unit, s)
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("%w: %q", ErrInvalidByteSize, s)
	}
	return ByteSize(f * float64(mult)), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (b *ByteSize) UnmarshalText(text []byte) error {
	v, err := ParseByteSize(string(text))
	if err != nil {
		return err
	}
	*b = v
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (b ByteSize) MarshalText() ([]byte, error) { return []byte(b.String()), nil }

// String renders the size with the largest binary unit that divides it
// exactly, falling back to bytes.
func (b ByteSize) String() string {
	for _, u := range []struct {
		name string
		size int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if b != 0 && int64(b)%u.size == 0 {
			return strconv.FormatInt(int64(b)/u.size, 10) + u.name
		}
	}
	return strconv.FormatInt(int64(b), 10)
}

package config

import (
	"errors"
	"testing"
)

func TestParseByteSize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want ByteSize
	}{
		{"0", 0},
		{"1024", 1024},
		{"1KiB", 1024},
		{"512MiB", 512 << 20},
		{"50GiB", 50 << 30},
		{"1 TiB", 1 << 40},
		{"20GB", 20_000_000_000},
		{"1.5KiB", 1536},
		{"2mb", 2_000_000},
	}
	for _, tt := range tests {
		got, err := ParseByteSize(tt.in)
		if err != nil {
			t.Errorf("ParseByteSize(%q) error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseByteSize(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestParseByteSizeRejectsGarbage(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "abc", "10XB", "-1", "1..5GiB"} {
		if _, err := ParseByteSize(in); !errors.Is(err, ErrInvalidByteSize) {
			t.Errorf("ParseByteSize(%q) error = %v, want ErrInvalidByteSize", in, err)
		}
	}
}

func TestByteSizeRoundTrip(t *testing.T) {
	t.Parallel()
	var b ByteSize
	if err := b.UnmarshalText([]byte("3GiB")); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != "3GiB" {
		t.Errorf("String() = %q, want 3GiB", got)
	}
	if got := ByteSize(1500).String(); got != "1500" {
		t.Errorf("String() = %q, want 1500", got)
	}
}

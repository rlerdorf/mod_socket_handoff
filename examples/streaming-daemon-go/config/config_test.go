package config

import (
	"testing"
)

func TestParseMemLimit(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int64
	}{
		// Valid: bytes (no suffix)
		{"bare number", "1024", 1024},
		{"B suffix", "512B", 512},
		{"b lowercase", "512b", 512},

		// Valid: KiB
		{"KiB", "1KiB", 1024},
		{"K shorthand", "4K", 4096},
		{"KB", "2KB", 2048},

		// Valid: MiB
		{"MiB", "768MiB", 768 * 1024 * 1024},
		{"M shorthand", "512M", 512 * 1024 * 1024},
		{"MB", "256MB", 256 * 1024 * 1024},

		// Valid: GiB
		{"GiB", "1GiB", 1024 * 1024 * 1024},
		{"G shorthand", "2G", 2 * 1024 * 1024 * 1024},
		{"GB", "4GB", 4 * 1024 * 1024 * 1024},

		// Valid: TiB
		{"TiB", "1TiB", 1024 * 1024 * 1024 * 1024},
		{"T shorthand", "2T", 2 * 1024 * 1024 * 1024 * 1024},
		{"TB", "1TB", 1024 * 1024 * 1024 * 1024},

		// Valid: case insensitivity
		{"mib lowercase", "512mib", 512 * 1024 * 1024},
		{"GIB uppercase", "1GIB", 1024 * 1024 * 1024},
		{"MiB mixed case", "256Mib", 256 * 1024 * 1024},

		// Valid: whitespace handling
		{"leading space", "  512MiB", 512 * 1024 * 1024},
		{"trailing space", "512MiB  ", 512 * 1024 * 1024},
		{"space between number and suffix", "512 MiB", 512 * 1024 * 1024},

		// Valid: large values within int64 range
		{"8 TiB", "8TiB", 8 * 1024 * 1024 * 1024 * 1024},
		{"max safe GiB", "8388607GiB", 8388607 * 1024 * 1024 * 1024}, // ~8 PiB, fits int64

		// Invalid: zero (treated as error; callers reject <= 0)
		{"zero", "0", -1},

		// Invalid: empty / whitespace-only
		{"empty string", "", -1},
		{"whitespace only", "   ", -1},

		// Invalid: no numeric part
		{"suffix only", "MiB", -1},
		{"just letters", "abc", -1},

		// Invalid: bad suffix
		{"unknown suffix", "512XiB", -1},
		{"random suffix", "512foo", -1},

		// Invalid: negative numbers (strconv.ParseUint rejects these)
		{"negative", "-512MiB", -1},

		// Invalid: fractional values (not supported)
		{"fractional", "1.5GiB", -1},
		{"decimal point only", ".5GiB", -1},

		// Invalid: overflow
		{"overflow TiB", "9999999TiB", -1},
		{"max uint64 bytes", "18446744073709551615", -1}, // MaxUint64 > MaxInt64
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseMemLimit(tc.input)
			if got != tc.want {
				t.Errorf("ParseMemLimit(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

func TestValidateMemLimit(t *testing.T) {
	tests := []struct {
		name    string
		limit   string
		wantErr bool
	}{
		{"empty is valid", "", false},
		{"valid MiB", "512MiB", false},
		{"valid GiB", "1GiB", false},
		{"invalid suffix", "512XiB", true},
		{"zero", "0", true},
		{"negative", "-1MiB", true},
		{"overflow", "9999999TiB", true},
		{"no number", "MiB", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Server.MemLimit = tc.limit
			err := cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateGCPercent(t *testing.T) {
	tests := []struct {
		name      string
		gcPercent int
		wantErr   bool
	}{
		{"zero is valid", 0, false},
		{"positive is valid", 100, false},
		{"large value is valid", 1000, false},
		{"negative is invalid", -1, true},
		{"very negative is invalid", -100, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Server.GCPercent = tc.gcPercent
			err := cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestParseDuration(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "30s", want: 30 * time.Second},
		{in: "5m", want: 5 * time.Minute},
		{in: "2h30m", want: 2*time.Hour + 30*time.Minute},

		{in: "1d", want: 24 * time.Hour},
		{in: "2w", want: 336 * time.Hour},
		{in: "1d12h", want: 36 * time.Hour},

		{in: "300", want: 300 * time.Second},
		{in: "0", want: 0},

		{in: "-5m", want: -5 * time.Minute},
		{in: "+5m", want: 5 * time.Minute},

		{in: "1h30", wantErr: true},
		{in: "30x", wantErr: true},
		{in: "", wantErr: true},
		{in: "-", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseDuration(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseDuration(%q) = %v, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDuration(%q): unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseDuration(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestDurationUnmarshalYAML(t *testing.T) {
	type doc struct {
		D Duration `yaml:"d"`
	}

	tests := []struct {
		name    string
		yaml    string
		want    time.Duration
		wantErr bool
	}{
		{name: "integer is seconds", yaml: "d: 300", want: 300 * time.Second},
		{name: "zero integer", yaml: "d: 0", want: 0},
		{name: "negative integer is seconds", yaml: "d: -10", want: -10 * time.Second},
		{name: "go duration string", yaml: `d: "1h30m"`, want: 90 * time.Minute},
		{name: "unquoted duration string", yaml: "d: 5m", want: 5 * time.Minute},
		{name: "day unit string", yaml: "d: 1d", want: 24 * time.Hour},
		{name: "quoted bare number is seconds", yaml: `d: "45"`, want: 45 * time.Second},
		{name: "bad string errors", yaml: `d: "1h30"`, wantErr: true},
		{name: "non-scalar errors", yaml: "d: [1, 2]", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out doc
			err := yaml.Unmarshal([]byte(tt.yaml), &out)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Unmarshal(%q) = %v, want error", tt.yaml, out.D)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal(%q): unexpected error: %v", tt.yaml, err)
			}
			if out.D.Std() != tt.want {
				t.Errorf("Unmarshal(%q) = %v, want %v", tt.yaml, out.D.Std(), tt.want)
			}
		})
	}
}

package main

import (
	"reflect"
	"testing"
)

func TestExpandCountFlags(t *testing.T) {
	countNames := map[string]bool{"v": true, "t": true}

	tests := []struct {
		name  string
		input []string
		want  []string
		// wantVerbose / wantTraffic: expected counts after parsing (derived from want).
		wantVerbose int
		wantTraffic int
	}{
		{
			name:        "single -v",
			input:       []string{"-v"},
			want:        []string{"-v"},
			wantVerbose: 1,
		},
		{
			name:        "-vv expands to two -v",
			input:       []string{"-vv"},
			want:        []string{"-v", "-v"},
			wantVerbose: 2,
		},
		{
			name:        "-vvv expands to three -v",
			input:       []string{"-vvv"},
			want:        []string{"-v", "-v", "-v"},
			wantVerbose: 3,
		},
		{
			name:        "-tt expands to two -t",
			input:       []string{"-tt"},
			want:        []string{"-t", "-t"},
			wantTraffic: 2,
		},
		{
			name:  "--verbose is a long flag, left untouched",
			input: []string{"--verbose"},
			want:  []string{"--verbose"},
		},
		{
			name:  "--listen-host untouched",
			input: []string{"--listen-host", "0.0.0.0"},
			want:  []string{"--listen-host", "0.0.0.0"},
		},
		{
			name:  "-c file untouched",
			input: []string{"-c", "file.ini"},
			want:  []string{"-c", "file.ini"},
		},
		{
			name:        "mixed: -vvv -c file.ini -tt",
			input:       []string{"-vvv", "-c", "file.ini", "-tt"},
			want:        []string{"-v", "-v", "-v", "-c", "file.ini", "-t", "-t"},
			wantVerbose: 3,
			wantTraffic: 2,
		},
		{
			name:  "non-count multi-char short flag untouched (e.g. -bb is not a count flag)",
			input: []string{"-bb"},
			want:  []string{"-bb"},
		},
		{
			name:  "mixed count chars like -vt left untouched",
			input: []string{"-vt"},
			want:  []string{"-vt"},
		},
		{
			name:  "empty args",
			input: []string{},
			want:  []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := expandCountFlags(tc.input, countNames)
			if len(tc.want) == 0 && len(got) == 0 {
				return // both empty — OK
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("expandCountFlags(%v) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

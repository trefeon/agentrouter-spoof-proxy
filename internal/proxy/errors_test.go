package proxy

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrorSentinels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"timeout", ErrTimeout, "EUPSTREAM_TIMEOUT"},
		{"upstream", ErrUpstream, "EUPSTREAM"},
		{"circuit", ErrCircuit, "ECIRCUIT_OPEN"},
		{"internal", ErrInternal, "EINTERNAL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err.Error() != tc.want {
				t.Errorf("Error() = %q, want %q", tc.err.Error(), tc.want)
			}
		})
	}
}

func TestCodeToStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"timeout", ErrTimeout, 504},
		{"upstream", ErrUpstream, 502},
		{"circuit", ErrCircuit, 503},
		{"internal", ErrInternal, 500},
		{"wrapped timeout", fmt.Errorf("upstream: %w", ErrTimeout), 504},
		{"wrapped upstream", fmt.Errorf("dial: %w", ErrUpstream), 502},
		{"wrapped circuit", fmt.Errorf("breaker: %w", ErrCircuit), 503},
		{"unknown error", errors.New("boom"), 500},
		{"nil", nil, 500},
		{"wrapped unknown", fmt.Errorf("cause: %w", errors.New("boom")), 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CodeToStatus(tc.err); got != tc.want {
				t.Errorf("CodeToStatus(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

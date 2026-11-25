package main

import (
	"testing"
	"time"
)

func TestPSRPCSelectTimeout(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"unset uses default", "", defaultPSRPCSelectTimeout},
		{"valid override", "3s", 3 * time.Second},
		{"invalid falls back", "banana", defaultPSRPCSelectTimeout},
		{"zero falls back", "0s", defaultPSRPCSelectTimeout},
		{"negative falls back", "-5s", defaultPSRPCSelectTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envPSRPCSelectTimeout, tc.env)
			if got := psrpcSelectTimeout(); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

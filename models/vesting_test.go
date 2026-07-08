package models

import (
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestBuildVestingSignMessageIncludesType(t *testing.T) {
	message := BuildVestingSignMessage(VestingSignPayload{
		Address:      "0x1111111111111111111111111111111111111111",
		TotalAmount:  "1000",
		StartTime:    1767225600,
		DurationDays: 30,
		Type:         VestingTypeNode,
	})

	if !strings.HasSuffix(message, "\nType: node") {
		t.Fatalf("expected vesting sign message to include type, got %q", message)
	}
}

func TestComputeVestingShouldReleased(t *testing.T) {
	total := big.NewInt(0).SetUint64(1000)
	start := time.Date(2026, 1, 1, 13, 30, 0, 0, time.UTC)

	tests := []struct {
		name     string
		now      time.Time
		duration uint
		expected string
	}{
		{
			name:     "before start",
			now:      start.Add(-time.Second),
			duration: 10,
			expected: "0",
		},
		{
			name:     "same day no release",
			now:      time.Date(2026, 1, 1, 23, 59, 59, 0, time.UTC),
			duration: 10,
			expected: "0",
		},
		{
			name:     "next day midnight releases first day",
			now:      time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
			duration: 10,
			expected: "100",
		},
		{
			name:     "mid schedule",
			now:      time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC),
			duration: 10,
			expected: "300",
		},
		{
			name:     "non UTC input uses UTC calendar day",
			now:      time.Date(2026, 1, 2, 7, 59, 59, 0, time.FixedZone("UTC+8", 8*60*60)),
			duration: 10,
			expected: "0",
		},
		{
			name:     "final day and beyond",
			now:      start.Add(15 * 24 * time.Hour),
			duration: 10,
			expected: "1000",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeVestingShouldReleased(total, start, tc.duration, tc.now)
			if got.String() != tc.expected {
				t.Fatalf("expected %s, got %s", tc.expected, got.String())
			}
		})
	}
}

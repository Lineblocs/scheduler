package billing

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestComputeAmountToCharge(t *testing.T) {
	tests := []struct {
		name      string
		fullCents float64
		available float64
		used      float64
		want      float64
		wantErr   bool
	}{
		{
			name:      "enough available, no charge",
			fullCents: 100,
			available: 500,
			used:      100,
			want:      0,
		},
		{
			name:      "no available, full charge",
			fullCents: 100,
			available: 0,
			used:      100,
			want:      100,
		},
		{
			name:      "negative available, full charge",
			fullCents: 100,
			available: -10,
			used:      50,
			want:      100,
		},
		{
			name:      "partial available, prorated charge",
			fullCents: 100,
			available: 50,
			used:      100,
			want:      50,
		},
		{
			name:      "partial charge minimum 1 cent",
			fullCents: 1,
			available: 50,
			used:      100,
			want:      1,
		},
		{
			name:      "exactly equal available and used",
			fullCents: 100,
			available: 100,
			used:      100,
			want:      0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := computeAmountToCharge(tt.fullCents, tt.available, tt.used)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.InDelta(t, tt.want, got, 1.0, "charge amount mismatch")
			}
		})
	}
}

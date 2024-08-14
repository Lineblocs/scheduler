package utils

import (
	"testing"

	helpers "github.com/Lineblocs/go-helpers"
	"github.com/stretchr/testify/assert"
)

func TestGetPlan(t *testing.T) {
	t.Parallel()

	helpers.InitLogrus("file")

	t.Run("Should return the correct plan name", func(t *testing.T) {
		t.Parallel()

		workspace := &helpers.Workspace{
			Plan: "starter",
		}

		plans := []helpers.ServicePlan{
			{
				Name: "starter",
			},
			{
				Name: "premium",
			},
		}

		plan := GetPlan(plans, workspace)
		assert.Equal(t, workspace.Plan, plan.Name)
	})

	t.Run("Should return empty plan", func(t *testing.T) {
		t.Parallel()

		workspace := &helpers.Workspace{
			Plan: "free",
		}

		plans := []helpers.ServicePlan{
			{
				Name: "starter",
			},
			{
				Name: "premium",
			},
		}

		plan := GetPlan(plans, workspace)
		assert.Empty(t, plan)
	})
}

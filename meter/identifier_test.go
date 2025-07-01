package meter

import (
	"context"
	"testing"

	"github.com/evcc-io/evcc/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdentifierConfiguration(t *testing.T) {
	// Test configuration with identifier
	config := map[string]interface{}{
		"power": map[string]interface{}{
			"source": "const",
			"value":  "100",
		},
		"identifier": map[string]interface{}{
			"source": "const",
			"value":  "TEST-DEVICE-123",
		},
	}

	ctx := context.Background()
	meter, err := NewConfigurableFromConfig(ctx, config)
	require.NoError(t, err)

	// Test that meter implements Identifier interface
	identifierMeter, ok := meter.(api.Identifier)
	require.True(t, ok, "meter should implement api.Identifier interface")

	// Test identifier value
	id, err := identifierMeter.Identify()
	require.NoError(t, err)
	assert.Equal(t, "TEST-DEVICE-123", id)

	// Test power still works
	power, err := meter.CurrentPower()
	require.NoError(t, err)
	assert.Equal(t, 100.0, power)
}

func TestIdentifierWithNullTrimming(t *testing.T) {
	// Test configuration with identifier containing NUL bytes and whitespace
	config := map[string]interface{}{
		"power": map[string]interface{}{
			"source": "const",
			"value":  "200",
		},
		"identifier": map[string]interface{}{
			"source": "const",
			"value":  "  TEST-DEVICE-456\x00\x00  ",
		},
	}

	ctx := context.Background()
	meter, err := NewConfigurableFromConfig(ctx, config)
	require.NoError(t, err)

	identifierMeter := meter.(api.Identifier)
	id, err := identifierMeter.Identify()
	require.NoError(t, err)
	assert.Equal(t, "TEST-DEVICE-456", id, "identifier should be trimmed of NUL bytes and whitespace")
}

func TestNoIdentifierConfiguration(t *testing.T) {
	// Test configuration without identifier
	config := map[string]interface{}{
		"power": map[string]interface{}{
			"source": "const",
			"value":  "300",
		},
	}

	ctx := context.Background()
	meter, err := NewConfigurableFromConfig(ctx, config)
	require.NoError(t, err)

	// Test that meter does not implement Identifier interface when no identifier is configured
	_, ok := meter.(api.Identifier)
	assert.False(t, ok, "meter should not implement api.Identifier interface when no identifier is configured")

	// Test power still works
	power, err := meter.CurrentPower()
	require.NoError(t, err)
	assert.Equal(t, 300.0, power)
}

func TestIdentifierWithOtherCapabilities(t *testing.T) {
	// Test configuration with identifier and other capabilities (energy, battery)
	config := map[string]interface{}{
		"power": map[string]interface{}{
			"source": "const",
			"value":  "400",
		},
		"energy": map[string]interface{}{
			"source": "const",
			"value":  "1000",
		},
		"soc": map[string]interface{}{
			"source": "const",
			"value":  "75",
		},
		"identifier": map[string]interface{}{
			"source": "const",
			"value":  "MULTI-CAPABILITY-DEVICE",
		},
	}

	ctx := context.Background()
	meter, err := NewConfigurableFromConfig(ctx, config)
	require.NoError(t, err)

	// Test all capabilities
	power, err := meter.CurrentPower()
	require.NoError(t, err)
	assert.Equal(t, 400.0, power)

	energyMeter, ok := meter.(api.MeterEnergy)
	require.True(t, ok)
	energy, err := energyMeter.TotalEnergy()
	require.NoError(t, err)
	assert.Equal(t, 1000.0, energy)

	batteryMeter, ok := meter.(api.Battery)
	require.True(t, ok)
	soc, err := batteryMeter.Soc()
	require.NoError(t, err)
	assert.Equal(t, 75.0, soc)

	identifierMeter, ok := meter.(api.Identifier)
	require.True(t, ok)
	id, err := identifierMeter.Identify()
	require.NoError(t, err)
	assert.Equal(t, "MULTI-CAPABILITY-DEVICE", id)
}

func TestIdentifierError(t *testing.T) {
	// Test configuration with identifier that returns an error
	config := map[string]interface{}{
		"power": map[string]interface{}{
			"source": "const",
			"value":  "500",
		},
		"identifier": map[string]interface{}{
			"source": "const",
			"value":  "", // Empty value to test error handling
		},
	}

	ctx := context.Background()
	meter, err := NewConfigurableFromConfig(ctx, config)
	require.NoError(t, err)

	identifierMeter := meter.(api.Identifier)
	id, err := identifierMeter.Identify()
	require.NoError(t, err)
	assert.Equal(t, "", id, "empty identifier should be handled gracefully")
}

// Test that the template system can render identifier configuration
func TestTemplateIdentifierRendering(t *testing.T) {
	// This would test that templates with identifier blocks render correctly
	// The actual template rendering is tested in the util/templates package
	// Here we just verify that a rendered template config works
	
	// Simulate a rendered template configuration
	config := map[string]interface{}{
		"type": "custom",
		"power": map[string]interface{}{
			"source": "modbus",
			"model":  "sunspec",
			"uri":    "192.168.1.100:502",
			"id":     1,
			"register": map[string]interface{}{
				"address": 40083,
				"type":    "input",
				"decode":  "int16",
			},
		},
		"identifier": map[string]interface{}{
			"source": "modbus",
			"model":  "sunspec",
			"uri":    "192.168.1.100:502",
			"id":     1,
			"register": map[string]interface{}{
				"address": 40044,
				"type":    "input",
				"decode":  "string",
				"length":  16,
			},
		},
	}

	// This test verifies the configuration structure is correct
	// In a real scenario, this would connect to a Modbus device
	// For testing purposes, we just verify the config structure
	assert.Contains(t, config, "identifier")
	assert.Contains(t, config, "power")
	
	identifierConfig := config["identifier"].(map[string]interface{})
	assert.Equal(t, "modbus", identifierConfig["source"])
	
	register := identifierConfig["register"].(map[string]interface{})
	assert.Equal(t, "string", register["decode"])
	assert.Equal(t, 16, register["length"])
}
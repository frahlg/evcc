package meter

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/plugin/mqtt"
	"github.com/evcc-io/evcc/util"
)

// Ferroamp EnergyHub MQTT meter
// Subscribes to extapi/data/ehub (grid, PV) and extapi/data/eso (battery)
// Default MQTT credentials: extapi / ferroampExtApi
// Ferroamp payload format: {"key": {"val": value}} or {"key": {"L1": v1, "L2": v2, "L3": v3}}
// Energy counters are in mJ (millijoules); converted to Wh: mJ / 3,600,000

func init() {
	registry.Add("ferroamp", NewFerroampFromConfig)
}

// ferroampData holds the latest JSON data from the EnergyHub
type ferroampData struct {
	mu   sync.RWMutex
	ehub map[string]any // extapi/data/ehub
	eso  map[string]any // extapi/data/eso
}

// Ferroamp meter implementation
type Ferroamp struct {
	log   *util.Logger
	usage string
	data  *ferroampData
	batteryCapacity
}

// NewFerroampFromConfig creates a Ferroamp EnergyHub meter from config
func NewFerroampFromConfig(other map[string]any) (api.Meter, error) {
	cc := struct {
		mqtt.Config     `mapstructure:",squash"`
		Usage           string
		Timeout         time.Duration
		batteryCapacity `mapstructure:",squash"`
	}{
		Timeout: 30 * time.Second,
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	usage := strings.ToLower(cc.Usage)
	if usage != "grid" && usage != "pv" && usage != "battery" {
		return nil, fmt.Errorf("invalid usage: %s (must be grid, pv, or battery)", cc.Usage)
	}

	return NewFerroamp(cc.Config, usage, cc.Timeout, cc.batteryCapacity)
}

// NewFerroamp creates Ferroamp EnergyHub meter
func NewFerroamp(mqttconf mqtt.Config, usage string, timeout time.Duration, batCap batteryCapacity) (api.Meter, error) {
	log := util.NewLogger("ferroamp")

	client, err := mqtt.RegisteredClient(log, mqttconf.Broker, mqttconf.User, mqttconf.Password,
		mqtt.ClientID(), 1, mqttconf.Insecure, mqttconf.CaCert, mqttconf.ClientCert, mqttconf.ClientKey)
	if err != nil {
		return nil, err
	}

	data := &ferroampData{
		ehub: make(map[string]any),
		eso:  make(map[string]any),
	}

	// subscribe to ehub data (grid power, PV, frequency, voltages, currents)
	if err := client.Listen("extapi/data/ehub", func(payload string) {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(payload), &parsed); err == nil {
			data.mu.Lock()
			data.ehub = parsed
			data.mu.Unlock()
		}
	}); err != nil {
		return nil, fmt.Errorf("subscribe ehub: %w", err)
	}

	// subscribe to eso data (battery SoC, voltage, current)
	if err := client.Listen("extapi/data/eso", func(payload string) {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(payload), &parsed); err == nil {
			data.mu.Lock()
			data.eso = parsed
			data.mu.Unlock()
		}
	}); err != nil {
		return nil, fmt.Errorf("subscribe eso: %w", err)
	}

	m := &Ferroamp{
		log:             log,
		usage:           usage,
		data:            data,
		batteryCapacity: batCap,
	}

	power := m.currentPower

	base, _ := NewConfigurable(power)

	switch usage {
	case "grid":
		return base.Decorate(
			m.totalEnergy,
			m.currents,
			m.voltages,
			nil, // no per-phase powers
			nil, // no max AC power
		), nil

	case "pv":
		return base.Decorate(
			nil,
			nil,
			nil,
			nil,
			nil,
		), nil

	case "battery":
		return base.DecorateBattery(
			nil,
			m.soc,
			m.batteryCapacity.Decorator(),
			nil, nil, nil,
		), nil

	default:
		return nil, fmt.Errorf("invalid usage: %s", usage)
	}
}

// extractVal extracts a value from Ferroamp's {"key": {"val": v}} structure
func extractVal(data map[string]any, key string) (any, bool) {
	field, ok := data[key]
	if !ok {
		return nil, false
	}
	if m, ok := field.(map[string]any); ok {
		if val, ok := m["val"]; ok {
			return val, true
		}
	}
	return field, true
}

// toFloat converts any value to float64 (handles both numeric and string JSON values)
func toFloat(val any) float64 {
	switch v := val.(type) {
	case float64:
		return v
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err == nil {
			return f
		}
	}
	return 0
}

// extractFloat extracts a float64 value
func extractFloat(data map[string]any, key string) float64 {
	val, ok := extractVal(data, key)
	if !ok {
		return 0
	}
	return toFloat(val)
}

// extractPhaseVal extracts a per-phase value from {"L1": v1, "L2": v2, "L3": v3}
func extractPhaseVal(data map[string]any, key, phase string) float64 {
	field, ok := data[key]
	if !ok {
		return 0
	}
	m, ok := field.(map[string]any)
	if !ok {
		return 0
	}

	// try {"val": {"L1":..}} or {"L1":..} directly
	inner := m
	if val, ok := m["val"]; ok {
		if vm, ok := val.(map[string]any); ok {
			inner = vm
		}
	}

	if v, ok := inner[phase]; ok {
		return toFloat(v)
	}
	return 0
}

// sumPhases sums L1+L2+L3 from a phase table
func sumPhases(data map[string]any, key string) float64 {
	return extractPhaseVal(data, key, "L1") +
		extractPhaseVal(data, key, "L2") +
		extractPhaseVal(data, key, "L3")
}

// mjToWh converts millijoules to watt-hours
func mjToWh(mj float64) float64 {
	return mj / 3_600_000
}

// currentPower implements the api.Meter interface
func (m *Ferroamp) currentPower() (float64, error) {
	m.data.mu.RLock()
	defer m.data.mu.RUnlock()

	switch m.usage {
	case "grid":
		// pext: per-phase grid power (W), positive=import, negative=export
		return sumPhases(m.data.ehub, "pext"), nil

	case "pv":
		// ppv: total solar power (W), Ferroamp reports as positive
		// evcc convention: PV power is positive
		return extractFloat(m.data.ehub, "ppv"), nil

	case "battery":
		// pbat: battery power (W)
		// Ferroamp: positive=discharge, negative=charge
		// evcc convention: positive=charge, negative=discharge
		bat := extractFloat(m.data.ehub, "pbat")
		return -bat, nil
	}

	return 0, fmt.Errorf("invalid usage: %s", m.usage)
}

// totalEnergy implements the api.MeterEnergy interface (grid usage)
func (m *Ferroamp) totalEnergy() (float64, error) {
	m.data.mu.RLock()
	defer m.data.mu.RUnlock()

	// wextconsq_3p: total grid import energy in mJ
	mj := extractFloat(m.data.ehub, "wextconsq_3p")
	return mjToWh(mj) / 1000, nil // Wh -> kWh
}

// currents implements the api.PhaseCurrents interface
func (m *Ferroamp) currents() (float64, float64, float64, error) {
	m.data.mu.RLock()
	defer m.data.mu.RUnlock()

	l1 := extractPhaseVal(m.data.ehub, "il", "L1")
	l2 := extractPhaseVal(m.data.ehub, "il", "L2")
	l3 := extractPhaseVal(m.data.ehub, "il", "L3")

	return l1, l2, l3, nil
}

// voltages implements the api.PhaseVoltages interface
func (m *Ferroamp) voltages() (float64, float64, float64, error) {
	m.data.mu.RLock()
	defer m.data.mu.RUnlock()

	l1 := extractPhaseVal(m.data.ehub, "ul", "L1")
	l2 := extractPhaseVal(m.data.ehub, "ul", "L2")
	l3 := extractPhaseVal(m.data.ehub, "ul", "L3")

	return l1, l2, l3, nil
}

// soc implements the api.Battery interface (battery usage)
func (m *Ferroamp) soc() (float64, error) {
	m.data.mu.RLock()
	defer m.data.mu.RUnlock()

	soc := extractFloat(m.data.eso, "soc")
	if soc == 0 {
		return 0, api.ErrNotAvailable
	}
	return soc, nil // already in %
}

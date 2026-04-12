package charger

// LICENSE

// Copyright (c) evcc.io (andig, naltatis, premultiply)

// This module is NOT covered by the MIT license. All rights reserved.

// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

// Ferroamp DC2 V2X 20 kW bidirectional DC charger (CCS2)
// Connected to Ferroamp EnergyHub's 760V DC nanogrid.
// MQTT broker runs on the DC2 charger itself, port 1883.
// Default credentials: dc2 / dc2mqtt
// The charger must be in Manual mode for external MQTT control.

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/plugin/mqtt"
	"github.com/evcc-io/evcc/util"
)

// FerroampDC2 charger implementation
type FerroampDC2 struct {
	log     *util.Logger
	client  *mqtt.Client
	topic   string  // base topic prefix, default "dc2"
	mu      sync.RWMutex
	state   map[string]string // cached values by key
	power   float64           // last set target power in kW
	enabled bool
	updated time.Time
}

func init() {
	registry.Add("ferroamp-dc2", NewFerroampDC2FromConfig)
}

// NewFerroampDC2FromConfig creates a Ferroamp DC2 charger from generic config
func NewFerroampDC2FromConfig(other map[string]any) (api.Charger, error) {
	cc := struct {
		mqtt.Config `mapstructure:",squash"`
		Topic       string
		Connector   int
		Timeout     time.Duration
	}{
		Topic:     "dc2",
		Connector: 1,
		Timeout:   30 * time.Second,
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	return NewFerroampDC2(cc.Config, cc.Topic, cc.Connector, cc.Timeout)
}

// NewFerroampDC2 creates Ferroamp DC2 charger
func NewFerroampDC2(mqttconf mqtt.Config, topic string, connector int, timeout time.Duration) (*FerroampDC2, error) {
	log := util.NewLogger("ferroamp-dc2")

	client, err := mqtt.RegisteredClient(log, mqttconf.Broker, mqttconf.User, mqttconf.Password,
		mqtt.ClientID(), 1, mqttconf.Insecure, mqttconf.CaCert, mqttconf.ClientCert, mqttconf.ClientKey)
	if err != nil {
		return nil, err
	}

	wb := &FerroampDC2{
		log:    log,
		client: client,
		topic:  topic,
		state:  make(map[string]string),
	}

	prefix := fmt.Sprintf("%s/connector/%d", topic, connector)

	// subscribe to individual charger data topics
	topics := map[string]string{
		prefix + "/pe/measured_voltage":      "pe/measured_voltage",
		prefix + "/pe/measured_current":      "pe/measured_current",
		prefix + "/ev/soc":                   "ev/soc",
		prefix + "/ev/id_state":              "ev/id_state",
		prefix + "/ev/limits/max_power":      "ev/limits/max_power",
		prefix + "/ev/limits/min_power":      "ev/limits/min_power",
		prefix + "/ev/limits/max_discharge_power": "ev/limits/max_discharge_power",
		prefix + "/em/transferred_energy":    "em/transferred_energy",
	}

	for mqttTopic, stateKey := range topics {
		key := stateKey // capture for closure
		if err := client.Listen(mqttTopic, func(payload string) {
			wb.mu.Lock()
			defer wb.mu.Unlock()
			wb.state[key] = strings.TrimSpace(payload)
			wb.updated = time.Now()
		}); err != nil {
			return nil, fmt.Errorf("subscribe %s: %w", mqttTopic, err)
		}
	}

	return wb, nil
}

// getFloat returns a float64 from the state map, or 0 if not present
func (wb *FerroampDC2) getFloat(key string) float64 {
	wb.mu.RLock()
	defer wb.mu.RUnlock()
	if v, ok := wb.state[key]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return 0
}

// getString returns a string from the state map
func (wb *FerroampDC2) getString(key string) string {
	wb.mu.RLock()
	defer wb.mu.RUnlock()
	return wb.state[key]
}

// Status implements the api.Charger interface
func (wb *FerroampDC2) Status() (api.ChargeStatus, error) {
	idState := wb.getString("ev/id_state")

	switch idState {
	case "mated", "mated_ev_aux", "mated_evse_aux":
		// EV connected - check if actively charging
		current := wb.getFloat("pe/measured_current")
		if current != 0 && wb.enabled {
			return api.StatusC, nil
		}
		return api.StatusB, nil
	default:
		return api.StatusA, nil
	}
}

// Enabled implements the api.Charger interface
func (wb *FerroampDC2) Enabled() (bool, error) {
	return wb.enabled, nil
}

// Enable implements the api.Charger interface
func (wb *FerroampDC2) Enable(enable bool) error {
	if enable {
		wb.enabled = true
		if wb.power == 0 {
			wb.power = 1.0 // 1 kW minimum
		}
		return wb.setPower(wb.power)
	}

	wb.enabled = false
	return wb.setPower(0)
}

// setPower publishes the power setpoint in kW to the DC2 control topic.
// Positive = charge (grid to car), negative = discharge (car to grid).
func (wb *FerroampDC2) setPower(powerKW float64) error {
	payload := fmt.Sprintf("%.2f", powerKW)
	wb.client.Publish(fmt.Sprintf("%s/ui/control", wb.topic), false, payload)
	return nil
}

// MaxCurrent implements the api.Charger interface
func (wb *FerroampDC2) MaxCurrent(current int64) error {
	return wb.MaxCurrentMillis(float64(current))
}

var _ api.ChargerEx = (*FerroampDC2)(nil)

// MaxCurrentMillis implements the api.ChargerEx interface.
// The DC2 is power-controlled (kW), so we convert the AC current limit to power.
func (wb *FerroampDC2) MaxCurrentMillis(current float64) error {
	if current < 6 {
		return fmt.Errorf("invalid current %.1f", current)
	}

	// convert AC current to power: P = I * V * phases
	// evcc sends current per phase, typical 3-phase 230V
	power := current * 230 * 3 / 1000 // W -> kW

	// cap at DC2 rated max of 20 kW
	if power > 20 {
		power = 20
	}

	wb.power = power
	if !wb.enabled {
		return nil
	}

	return wb.setPower(power)
}

var _ api.Meter = (*FerroampDC2)(nil)

// CurrentPower implements the api.Meter interface
func (wb *FerroampDC2) CurrentPower() (float64, error) {
	voltage := wb.getFloat("pe/measured_voltage")
	current := wb.getFloat("pe/measured_current")
	return voltage * current, nil // W
}

var _ api.Battery = (*FerroampDC2)(nil)

// Soc implements the api.Battery interface
func (wb *FerroampDC2) Soc() (float64, error) {
	soc := wb.getFloat("ev/soc")
	if soc == 0 {
		return 0, api.ErrNotAvailable
	}
	return soc, nil
}

var _ api.ChargeRater = (*FerroampDC2)(nil)

// ChargedEnergy implements the api.ChargeRater interface
func (wb *FerroampDC2) ChargedEnergy() (float64, error) {
	// transferred_energy is in kWh from the vSECC
	return wb.getFloat("em/transferred_energy"), nil
}

var _ api.Diagnosis = (*FerroampDC2)(nil)

// Diagnose implements the api.Diagnosis interface
func (wb *FerroampDC2) Diagnose() {
	fmt.Printf("\tDC Voltage:\t%.1f V\n", wb.getFloat("pe/measured_voltage"))
	fmt.Printf("\tDC Current:\t%.2f A\n", wb.getFloat("pe/measured_current"))
	fmt.Printf("\tEV SoC:\t\t%.0f %%\n", wb.getFloat("ev/soc"))
	fmt.Printf("\tID State:\t%s\n", wb.getString("ev/id_state"))
	fmt.Printf("\tMax Power:\t%.0f W\n", wb.getFloat("ev/limits/max_power"))
	fmt.Printf("\tMin Power:\t%.0f W\n", wb.getFloat("ev/limits/min_power"))
	fmt.Printf("\tMax Discharge:\t%.0f W\n", wb.getFloat("ev/limits/max_discharge_power"))
	fmt.Printf("\tSession Energy:\t%.2f kWh\n", wb.getFloat("em/transferred_energy"))
	fmt.Printf("\tEnabled:\t%v\n", wb.enabled)
	fmt.Printf("\tTarget Power:\t%.2f kW\n", wb.power)
}

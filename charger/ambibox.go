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

// Ambibox ambiCHARGE 11kW Home BiDi DC Wallbox
// Modbus TCP interface based on sidOS Modbus TCP Interface v1.20
// https://www.ambibox.de

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/modbus"
)

// Ambibox charger implementation
type Ambibox struct {
	log     *util.Logger
	conn    *modbus.Connection
	base    uint16 // input register base address for EV Charger (default 4000)
	wrBase  uint16 // holding register base address for EV Charger (default 3000)
	current float64
	enabled bool
}

const (
	// Input register offsets (read via ReadInputRegisters, base address 4000 for EV Charger 1)
	ambiRegSleep             = 0  // int32, 0=awake 1=asleep
	ambiRegCurrentAC         = 2  // float, A
	ambiRegCurrentACPhase1   = 4  // float, A
	ambiRegCurrentACPhase2   = 6  // float, A
	ambiRegCurrentACPhase3   = 8  // float, A
	ambiRegVoltageAC         = 10 // float, V
	ambiRegVoltageACPhase1   = 12 // float, V
	ambiRegVoltageACPhase2   = 14 // float, V
	ambiRegVoltageACPhase3   = 16 // float, V
	ambiRegPowerAC           = 18 // int32, W
	ambiRegGridFrequency     = 22 // float, Hz
	ambiRegEnergyAC          = 24 // float, Wh
	ambiRegEnergyACImport    = 26 // float, Wh
	ambiRegEnergyACExport    = 28 // float, Wh
	ambiRegVoltageDC         = 30 // float, V
	ambiRegCurrentDC         = 32 // float, A
	ambiRegPowerDC           = 34 // int32, W
	ambiRegNumPhases         = 36 // uint32
	ambiRegMinPowerAC        = 38 // int32, W
	ambiRegMaxPowerAC        = 40 // int32, W
	ambiRegInverterState     = 42 // enum(uint32)
	ambiRegInverterError     = 44 // enum(uint32)
	ambiRegInverterTemp      = 46 // float, °C
	ambiRegCapacity          = 48 // float, Wh
	ambiRegSoC               = 54 // float, %
	ambiRegMinChargePower    = 62 // uint32, W
	ambiRegMaxChargePower    = 64 // uint32, W
	ambiRegMinDischargePower = 66 // uint32, W
	ambiRegMaxDischargePower = 68 // uint32, W
	ambiRegBatteryState      = 70 // enum(uint32)
	ambiRegControlMode       = 76 // enum(uint32)
	ambiRegChargeProtocol    = 80 // enum(uint32)
	ambiRegSessionState      = 82 // enum(uint32)
	ambiRegEVConnected       = 84 // bool
	ambiRegSessionImport     = 92 // float, Wh
	ambiRegSessionExport     = 94 // float, Wh

	// Holding register offsets (write via WriteMultipleRegisters, base address 3000 for EV Charger 1)
	ambiWrTargetPowerAC = 0 // int32, W
	ambiWrWakeUp         = 2 // uint32, 1=wake up
	ambiWrSetSleep       = 4 // uint32, 1=sleep
	ambiWrStopCharge     = 6 // uint32, 1=stop

	// Session State (EvseState) enum values
	ambiSessionSetup     = 0
	ambiSessionAuth      = 1
	ambiSessionDiscovery = 2
	ambiSessionCableCheck = 3
	ambiSessionPreCharge = 4
	ambiSessionChargeLoop = 5
	ambiSessionPostCharge = 6
	ambiSessionPaused    = 7
	ambiSessionStopped   = 8
	ambiSessionError     = 9

	// Inverter State enum values
	ambiInvOffline      = 0
	ambiInvSleeping     = 1
	ambiInvStarting     = 2
	ambiInvMPPT         = 3
	ambiInvThrottled    = 4
	ambiInvShuttingDown = 5
	ambiInvFault        = 6
	ambiInvStandby      = 7
	ambiInvStarted      = 8

	// Control Mode enum values
	ambiControlDisabled     = 0
	ambiControlControllable = 1
	ambiControlLimitable    = 2
)

func init() {
	registry.AddCtx("ambibox", NewAmbiboxFromConfig)
}

// NewAmbiboxFromConfig creates an Ambibox charger from generic config
func NewAmbiboxFromConfig(ctx context.Context, other map[string]any) (api.Charger, error) {
	cc := struct {
		modbus.TcpSettings `mapstructure:",squash"`
		Connector          int
	}{
		TcpSettings: modbus.TcpSettings{
			ID: 1,
		},
		Connector: 1,
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	return NewAmbibox(ctx, cc.URI, cc.ID, cc.Connector)
}

// NewAmbibox creates Ambibox charger
func NewAmbibox(ctx context.Context, uri string, id uint8, connector int) (api.Charger, error) {
	conn, err := modbus.NewConnection(ctx, uri, "", "", 0, modbus.Tcp, id)
	if err != nil {
		return nil, err
	}

	log := util.NewLogger("ambibox")
	conn.Logger(log.TRACE)

	// Input registers: EV Charger base=4000, stride=200
	base := uint16(4000 + (connector-1)*200)
	// Holding registers: EV Charger base=3000, stride=100
	wrBase := uint16(3000 + (connector-1)*100)

	wb := &Ambibox{
		log:    log,
		conn:   conn,
		base:   base,
		wrBase: wrBase,
	}

	// verify connectivity by reading sleep register
	if _, err := wb.conn.ReadInputRegisters(wb.base+ambiRegSleep, 2); err != nil {
		return nil, fmt.Errorf("initial read: %w", err)
	}

	return wb, nil
}

// readFloat reads a 32-bit IEEE 754 float from two input registers
func (wb *Ambibox) readFloat(reg uint16) (float64, error) {
	b, err := wb.conn.ReadInputRegisters(wb.base+reg, 2)
	if err != nil {
		return 0, err
	}
	return float64(math.Float32frombits(binary.BigEndian.Uint32(b))), nil
}

// readInt32 reads a signed 32-bit integer from two input registers
func (wb *Ambibox) readInt32(reg uint16) (int32, error) {
	b, err := wb.conn.ReadInputRegisters(wb.base+reg, 2)
	if err != nil {
		return 0, err
	}
	return int32(binary.BigEndian.Uint32(b)), nil
}

// readUint32 reads an unsigned 32-bit integer from two input registers
func (wb *Ambibox) readUint32(reg uint16) (uint32, error) {
	b, err := wb.conn.ReadInputRegisters(wb.base+reg, 2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b), nil
}

// writeInt32 writes a signed 32-bit integer to two holding registers
func (wb *Ambibox) writeInt32(reg uint16, val int32) error {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(val))
	_, err := wb.conn.WriteMultipleRegisters(wb.wrBase+reg, 2, b)
	return err
}

// writeUint32 writes an unsigned 32-bit integer to two holding registers
func (wb *Ambibox) writeUint32(reg uint16, val uint32) error {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, val)
	_, err := wb.conn.WriteMultipleRegisters(wb.wrBase+reg, 2, b)
	return err
}

// Status implements the api.Charger interface
func (wb *Ambibox) Status() (api.ChargeStatus, error) {
	// check EV connected first
	evConn, err := wb.readUint32(ambiRegEVConnected)
	if err != nil {
		return api.StatusNone, err
	}

	if evConn == 0 {
		return api.StatusA, nil
	}

	// EV is connected - check session state
	session, err := wb.readUint32(ambiRegSessionState)
	if err != nil {
		return api.StatusNone, err
	}

	switch session {
	case ambiSessionChargeLoop:
		return api.StatusC, nil
	case ambiSessionPreCharge:
		return api.StatusC, nil
	case ambiSessionError:
		return api.StatusNone, fmt.Errorf("charger error state")
	default:
		// SESSION_SETUP, AUTHORIZATION, CHARGE_PARAMETER_DISCOVERY,
		// CABLE_CHECK, POST_CHARGE, PAUSED, STOPPED
		return api.StatusB, nil
	}
}

// Enabled implements the api.Charger interface
func (wb *Ambibox) Enabled() (bool, error) {
	return wb.enabled, nil
}

// Enable implements the api.Charger interface
func (wb *Ambibox) Enable(enable bool) error {
	if enable {
		// wake up the charger if sleeping
		if err := wb.writeUint32(ambiWrWakeUp, 1); err != nil {
			return err
		}
		wb.enabled = true
		// set the last known current target (or minimum)
		return wb.MaxCurrent(max(int64(wb.current), 6))
	}

	// disable: stop charging by setting target power to 0
	wb.enabled = false
	if err := wb.writeInt32(ambiWrTargetPowerAC, 0); err != nil {
		return err
	}
	return wb.writeUint32(ambiWrStopCharge, 1)
}

// MaxCurrent implements the api.Charger interface
func (wb *Ambibox) MaxCurrent(current int64) error {
	return wb.MaxCurrentMillis(float64(current))
}

var _ api.ChargerEx = (*Ambibox)(nil)

// MaxCurrentMillis implements the api.ChargerEx interface
func (wb *Ambibox) MaxCurrentMillis(current float64) error {
	if current < 6 {
		return fmt.Errorf("invalid current %.1f", current)
	}

	wb.current = current

	// convert current to power using actual voltage and phases
	voltage, err := wb.readFloat(ambiRegVoltageAC)
	if err != nil || voltage == 0 {
		voltage = 230 // fallback
	}

	phases, err := wb.readUint32(ambiRegNumPhases)
	if err != nil || phases == 0 {
		phases = 1 // fallback for DC
	}

	power := int32(current * voltage * float64(phases))

	return wb.writeInt32(ambiWrTargetPowerAC, power)
}

var _ api.Meter = (*Ambibox)(nil)

// CurrentPower implements the api.Meter interface
func (wb *Ambibox) CurrentPower() (float64, error) {
	p, err := wb.readInt32(ambiRegPowerAC)
	return float64(p), err
}

var _ api.MeterEnergy = (*Ambibox)(nil)

// TotalEnergy implements the api.MeterEnergy interface
func (wb *Ambibox) TotalEnergy() (float64, error) {
	e, err := wb.readFloat(ambiRegEnergyACImport)
	return e / 1000, err // Wh -> kWh
}

var _ api.PhaseCurrents = (*Ambibox)(nil)

// Currents implements the api.PhaseCurrents interface
func (wb *Ambibox) Currents() (float64, float64, float64, error) {
	l1, err := wb.readFloat(ambiRegCurrentACPhase1)
	if err != nil {
		return 0, 0, 0, err
	}
	l2, err := wb.readFloat(ambiRegCurrentACPhase2)
	if err != nil {
		return 0, 0, 0, err
	}
	l3, err := wb.readFloat(ambiRegCurrentACPhase3)
	if err != nil {
		return 0, 0, 0, err
	}
	return l1, l2, l3, nil
}

var _ api.PhaseVoltages = (*Ambibox)(nil)

// Voltages implements the api.PhaseVoltages interface
func (wb *Ambibox) Voltages() (float64, float64, float64, error) {
	l1, err := wb.readFloat(ambiRegVoltageACPhase1)
	if err != nil {
		return 0, 0, 0, err
	}
	l2, err := wb.readFloat(ambiRegVoltageACPhase2)
	if err != nil {
		return 0, 0, 0, err
	}
	l3, err := wb.readFloat(ambiRegVoltageACPhase3)
	if err != nil {
		return 0, 0, 0, err
	}
	return l1, l2, l3, nil
}

var _ api.Battery = (*Ambibox)(nil)

// Soc implements the api.Battery interface
func (wb *Ambibox) Soc() (float64, error) {
	return wb.readFloat(ambiRegSoC)
}

var _ api.ChargeRater = (*Ambibox)(nil)

// ChargedEnergy implements the api.ChargeRater interface
func (wb *Ambibox) ChargedEnergy() (float64, error) {
	e, err := wb.readFloat(ambiRegSessionImport)
	return e / 1000, err // Wh -> kWh
}

var _ api.SocLimiter = (*Ambibox)(nil)

// GetLimitSoc implements the api.SocLimiter interface
func (wb *Ambibox) GetLimitSoc() (int64, error) {
	// read the max SoC register from the EV charger section (offset 52)
	soc, err := wb.readFloat(52) // Max State of Charge
	if err != nil {
		return 0, err
	}
	return int64(soc), nil
}

var _ api.Diagnosis = (*Ambibox)(nil)

// Diagnose implements the api.Diagnosis interface
func (wb *Ambibox) Diagnose() {
	if p, err := wb.readInt32(ambiRegPowerAC); err == nil {
		fmt.Printf("\tPower AC:\t%d W\n", p)
	}
	if p, err := wb.readInt32(ambiRegPowerDC); err == nil {
		fmt.Printf("\tPower DC:\t%d W\n", p)
	}
	if v, err := wb.readFloat(ambiRegVoltageAC); err == nil {
		fmt.Printf("\tVoltage AC:\t%.1f V\n", v)
	}
	if v, err := wb.readFloat(ambiRegVoltageDC); err == nil {
		fmt.Printf("\tVoltage DC:\t%.1f V\n", v)
	}
	if a, err := wb.readFloat(ambiRegCurrentAC); err == nil {
		fmt.Printf("\tCurrent AC:\t%.2f A\n", a)
	}
	if a, err := wb.readFloat(ambiRegCurrentDC); err == nil {
		fmt.Printf("\tCurrent DC:\t%.2f A\n", a)
	}
	if n, err := wb.readUint32(ambiRegNumPhases); err == nil {
		fmt.Printf("\tPhases:\t\t%d\n", n)
	}
	if s, err := wb.readUint32(ambiRegSessionState); err == nil {
		fmt.Printf("\tSession State:\t%d\n", s)
	}
	if s, err := wb.readUint32(ambiRegInverterState); err == nil {
		fmt.Printf("\tInverter State:\t%d\n", s)
	}
	if s, err := wb.readUint32(ambiRegBatteryState); err == nil {
		fmt.Printf("\tBattery State:\t%d\n", s)
	}
	if s, err := wb.readUint32(ambiRegControlMode); err == nil {
		fmt.Printf("\tControl Mode:\t%d\n", s)
	}
	if s, err := wb.readFloat(ambiRegSoC); err == nil {
		fmt.Printf("\tSoC:\t\t%.1f %%\n", s)
	}
	if e, err := wb.readUint32(ambiRegEVConnected); err == nil {
		fmt.Printf("\tEV Connected:\t%d\n", e)
	}
	if c, err := wb.readFloat(ambiRegCapacity); err == nil {
		fmt.Printf("\tCapacity:\t%.0f Wh\n", c)
	}
	if p, err := wb.readUint32(ambiRegMaxChargePower); err == nil {
		fmt.Printf("\tMax Charge:\t%d W\n", p)
	}
	if p, err := wb.readUint32(ambiRegMaxDischargePower); err == nil {
		fmt.Printf("\tMax Discharge:\t%d W\n", p)
	}
	if cp, err := wb.readUint32(ambiRegChargeProtocol); err == nil {
		fmt.Printf("\tCharge Proto:\t%d\n", cp)
	}
}

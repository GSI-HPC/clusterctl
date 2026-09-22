// SPDX-License-Identifier: LGPL-3.0-or-later

package redfish

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// The reset types this program uses, named as the Redfish schema names them.
const (
	ResetOn               = "On"
	ResetForceOff         = "ForceOff"
	ResetGracefulShutdown = "GracefulShutdown"
	ResetForceRestart     = "ForceRestart"
	ResetGracefulRestart  = "GracefulRestart"
	ResetPowerCycle       = "PowerCycle"
	ResetForceOn          = "ForceOn"
	ResetNmi              = "Nmi"
)

// System is what a computer system reports about itself.
type System struct {
	Name         string `json:"name,omitempty" yaml:"name,omitempty"`
	PowerState   string `json:"powerState,omitempty" yaml:"powerState,omitempty"`
	Health       string `json:"health,omitempty" yaml:"health,omitempty"`
	State        string `json:"state,omitempty" yaml:"state,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty" yaml:"manufacturer,omitempty"`
	Model        string `json:"model,omitempty" yaml:"model,omitempty"`
	SerialNumber string `json:"serialNumber,omitempty" yaml:"serialNumber,omitempty"`
	BIOSVersion  string `json:"biosVersion,omitempty" yaml:"biosVersion,omitempty"`
	BootSource   string `json:"bootSource,omitempty" yaml:"bootSource,omitempty"`
	BootEnabled  string `json:"bootEnabled,omitempty" yaml:"bootEnabled,omitempty"`
	// ResetTypes are the reset types this machine says it accepts.
	ResetTypes []string `json:"resetTypes,omitempty" yaml:"resetTypes,omitempty"`
	// BootTargets are the boot sources it says it accepts.
	BootTargets []string `json:"bootTargets,omitempty" yaml:"bootTargets,omitempty"`
}

// System reads the computer system resource.
func (c *Client) System(ctx context.Context) (*System, error) {
	body, err := c.Get(ctx, c.system())
	if err != nil {
		return nil, err
	}
	out := &System{
		Name:         str(body["Name"]),
		PowerState:   str(body["PowerState"]),
		Manufacturer: str(body["Manufacturer"]),
		Model:        str(body["Model"]),
		SerialNumber: str(body["SerialNumber"]),
		BIOSVersion:  str(body["BiosVersion"]),
	}
	if status, ok := body["Status"].(map[string]any); ok {
		out.Health = str(status["Health"])
		out.State = str(status["State"])
	}
	if boot, ok := body["Boot"].(map[string]any); ok {
		out.BootSource = str(boot["BootSourceOverrideTarget"])
		out.BootEnabled = str(boot["BootSourceOverrideEnabled"])
		out.BootTargets = strList(boot["BootSourceOverrideTarget@Redfish.AllowableValues"])
	}
	out.ResetTypes = resetTypes(body)
	return out, nil
}

// resetTypes reads the reset types a system advertises.
func resetTypes(body map[string]any) []string {
	actions, ok := body["Actions"].(map[string]any)
	if !ok {
		return nil
	}
	reset, ok := actions["#ComputerSystem.Reset"].(map[string]any)
	if !ok {
		return nil
	}
	return strList(reset["ResetType@Redfish.AllowableValues"])
}

// resetTarget finds the URI the reset action is posted to, falling back to
// the conventional one when the firmware does not name it.
func (c *Client) resetTarget(body map[string]any) string {
	if actions, ok := body["Actions"].(map[string]any); ok {
		if reset, ok := actions["#ComputerSystem.Reset"].(map[string]any); ok {
			if target := str(reset["target"]); target != "" {
				return target
			}
		}
	}
	return strings.TrimRight(c.system(), "/") + "/Actions/ComputerSystem.Reset"
}

// PowerState reads the current power state.
func (c *Client) PowerState(ctx context.Context) (string, error) {
	body, err := c.Get(ctx, c.system())
	if err != nil {
		return "", err
	}
	state := str(body["PowerState"])
	if state == "" {
		return "", fmt.Errorf("%s reports no power state", c.Host)
	}
	return state, nil
}

// Reset asks the system to change power state.
//
// The reset type is checked against what the machine says it accepts, so that
// a type its firmware does not implement is reported here rather than as an
// opaque rejection. The request is sent exactly once: a reset that timed out
// may already have been carried out, and sending it again would power cycle a
// running machine.
func (c *Client) Reset(ctx context.Context, resetType string) error {
	body, err := c.Get(ctx, c.system())
	if err != nil {
		return err
	}
	allowed := resetTypes(body)
	if len(allowed) > 0 && !contains(allowed, resetType) {
		sorted := append([]string(nil), allowed...)
		sort.Strings(sorted)
		return fmt.Errorf("%s does not accept the reset type %q; it accepts %s",
			c.Host, resetType, strings.Join(sorted, ", "))
	}
	_, err = c.Post(ctx, c.resetTarget(body), map[string]any{"ResetType": resetType})
	return err
}

// SetBootOverride asks the system to boot from a given source next time.
//
// persistent selects whether the override survives the first boot;
// ClusterShell era tooling defaulted to persistent and left machines
// reinstalling in a loop, so the default here is once.
func (c *Client) SetBootOverride(ctx context.Context, target string, persistent bool) error {
	body, err := c.Get(ctx, c.system())
	if err != nil {
		return err
	}
	if boot, ok := body["Boot"].(map[string]any); ok {
		if allowed := strList(boot["BootSourceOverrideTarget@Redfish.AllowableValues"]); len(allowed) > 0 {
			if !contains(allowed, target) {
				sorted := append([]string(nil), allowed...)
				sort.Strings(sorted)
				return fmt.Errorf("%s does not accept the boot source %q; it accepts %s",
					c.Host, target, strings.Join(sorted, ", "))
			}
		}
	}
	enabled := "Once"
	if persistent {
		enabled = "Continuous"
	}
	_, err = c.Do(ctx, "PATCH", c.system(), map[string]any{
		"Boot": map[string]any{
			"BootSourceOverrideTarget":  target,
			"BootSourceOverrideEnabled": enabled,
		},
	})
	return err
}

// ClearBootOverride removes a boot source override.
func (c *Client) ClearBootOverride(ctx context.Context) error {
	_, err := c.Do(ctx, "PATCH", c.system(), map[string]any{
		"Boot": map[string]any{"BootSourceOverrideEnabled": "Disabled"},
	})
	return err
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strList(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

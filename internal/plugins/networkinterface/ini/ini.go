// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package ini implements the runtime side of INI (IronCore Network Interface), an
// exec-based contract between a machine runtime and a host networking plugin
// (e.g. the reference implementation inisix). For every machine network
// interface the runtime invokes the plugin executable with INI_COMMAND=ADD to
// prepare a host device for the attachment, and with INI_COMMAND=DEL to remove
// all attachment state again. The ADD result announces which kind of device the
// plugin prepared: a tap device is handed to the guest via <interface
// type='ethernet'> with managed='no' (see the Ethernet variant of the network
// interface plugin contract): libvirt performs no setup on it, the INI plugin
// owns the device end to end. An SR-IOV virtual function is passed through to
// the guest via <hostdev mode='subsystem' type='pci'> (see the HostDevice
// variant of the network interface plugin contract).
//
// The ADD request carries the name of the attachment and the hostname to
// assign the guest, derived from the user-facing machine name. DEL only
// carries the attachment name.
//
// Besides the device, the ADD result announces the addressing the plugin set
// up for the attachment. "prefixes" (required, at least one entry) lists the
// prefixes routed to the guest. "ips" (optional) lists the individual
// addresses the plugin allocated for the attachment and serves to the guest,
// e.g. the address its DHCPv6 server hands out when the guest requests just a
// single address as most clients do. Both lists are reported in the machine's
// network interface status, giving users the address to connect to or publish
// in DNS. The runtime cannot observe which addresses the guest actually
// configured, so "ips" always reflects what the plugin serves, never guest
// state.
package ini

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	machinepoolletv1alpha1 "github.com/ironcore-dev/ironcore/poollet/machinepoollet/api/v1alpha1"
	"github.com/ironcore-dev/libvirt-provider/api"
	providerhost "github.com/ironcore-dev/libvirt-provider/internal/host"
	providernetworkinterface "github.com/ironcore-dev/libvirt-provider/internal/plugins/networkinterface"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	PluginName = "ini"

	DefaultPluginPath = "/opt/ini/bin/inisix"

	execTimeout = 30 * time.Second

	perm = 0o777

	envCommand = "INI_COMMAND"

	commandAdd = "ADD"
	commandDel = "DEL"
)

type request struct {
	Name string `json:"name"`
	// Hostname is the hostname the plugin should assign the guest. It is only
	// sent with ADD and empty if the runtime cannot derive a sensible
	// hostname, in which case the plugin applies its own default.
	Hostname string `json:"hostname,omitempty"`
}

type addResult struct {
	Tap      *tapDevice `json:"tap"`
	VF       *vfDevice  `json:"vf"`
	Prefixes []string   `json:"prefixes"`
	Ips      []string   `json:"ips"`
}

type tapDevice struct {
	Name string `json:"name"`
}

type vfDevice struct {
	PCIAddress string `json:"pci-address"`
}

type errorResult struct {
	Error string `json:"error"`
}

type Plugin struct {
	pluginPath string
	host       providerhost.LibvirtHost
}

func NewPlugin(pluginPath string) providernetworkinterface.Plugin {
	if pluginPath == "" {
		pluginPath = DefaultPluginPath
	}
	return &Plugin{pluginPath: pluginPath}
}

func (p *Plugin) Name() string {
	return PluginName
}

func (p *Plugin) Init(_ context.Context, host providerhost.LibvirtHost) error {
	p.host = host
	return nil
}

func (p *Plugin) AddEventHandler(providernetworkinterface.EventHandler) {}

// AttachmentName derives the host-unique INI attachment name for a machine
// network interface. The name is a collision-resistant truncation of the
// SHA-256 hash of <machine-id>/<network-interface-name> and satisfies the INI
// name constraints: at most 15 characters, always matching
// ^[a-z0-9]([-a-z0-9]{0,13}[a-z0-9])?$.
func AttachmentName(machineID, networkInterfaceName string) string {
	sum := sha256.Sum256([]byte(machineID + "/" + networkInterfaceName))
	return hex.EncodeToString(sum[:])[:15]
}

func (p *Plugin) Apply(ctx context.Context, spec *api.NetworkInterfaceSpec, machine *api.Machine) (*providernetworkinterface.NetworkInterface, error) {
	log := ctrl.LoggerFrom(ctx)

	if err := os.MkdirAll(p.host.MachineNetworkInterfaceDir(machine.ID, spec.Name), perm); err != nil {
		return nil, err
	}

	name := AttachmentName(machine.ID, spec.Name)
	res := &addResult{}
	if err := p.run(ctx, commandAdd, request{Name: name, Hostname: hostname(machine)}, res); err != nil {
		return nil, err
	}
	if len(res.Prefixes) == 0 {
		return nil, fmt.Errorf("ini plugin ADD for attachment %q returned no prefixes", name)
	}
	prefixes := make([]string, 0, len(res.Prefixes))
	for _, p := range res.Prefixes {
		prefix, err := netip.ParsePrefix(p)
		if err != nil {
			return nil, fmt.Errorf("ini plugin ADD for attachment %q returned invalid prefix %q: %w",
				name, p, err)
		}
		prefixes = append(prefixes, prefix.String())
	}
	ips := make([]string, 0, len(res.Ips))
	for _, i := range res.Ips {
		ip, err := netip.ParseAddr(i)
		if err != nil {
			return nil, fmt.Errorf("ini plugin ADD for attachment %q returned invalid ip %q: %w",
				name, i, err)
		}
		ips = append(ips, ip.String())
	}

	// The ADD result must carry exactly one device member announcing which kind of
	// host device the plugin prepared. A result with no known device member (e.g.
	// from a plugin kind unknown to this runtime) must not be consumed.
	nic := &providernetworkinterface.NetworkInterface{
		Handle:   name,
		Prefixes: prefixes,
		Ips:      ips,
	}
	switch {
	case res.Tap != nil && res.VF != nil:
		return nil, fmt.Errorf("ini plugin ADD for attachment %q returned both tap and vf device members", name)
	case res.Tap != nil:
		if res.Tap.Name == "" {
			return nil, fmt.Errorf("ini plugin ADD for attachment %q returned a tap device without a name", name)
		}
		nic.Ethernet = &providernetworkinterface.Ethernet{Dev: res.Tap.Name}
		log.V(1).Info("Applied INI attachment", "attachment", name, "tapName", res.Tap.Name, "prefixes", prefixes, "ips", ips)
	case res.VF != nil:
		hostDevice, err := parsePCIAddress(res.VF.PCIAddress)
		if err != nil {
			return nil, fmt.Errorf("ini plugin ADD for attachment %q returned invalid vf pci-address %q: %w",
				name, res.VF.PCIAddress, err)
		}
		nic.HostDevice = hostDevice
		log.V(1).Info("Applied INI attachment", "attachment", name, "vfPciAddress", res.VF.PCIAddress, "prefixes", prefixes, "ips", ips)
	default:
		return nil, fmt.Errorf("ini plugin ADD for attachment %q returned no device member (tap or vf)", name)
	}
	return nic, nil
}

// hostname derives the hostname to assign the guest. An explicitly
// configured guest hostname takes precedence. Otherwise the name of the
// machine object the user created, which the machinepoollet records as a
// label on the IRI machine, is sanitized into a valid DNS label. An empty
// result means the runtime has no hostname to suggest.
func hostname(machine *api.Machine) string {
	if guestConfig := machine.Spec.GuestConfig; guestConfig != nil && guestConfig.HostName != "" {
		return guestConfig.HostName
	}
	return sanitizeHostname(machine.Metadata.Labels[machinepoolletv1alpha1.MachineNameLabel])
}

var invalidHostnameChars = regexp.MustCompile(`[^a-z0-9]+`)

// sanitizeHostname converts s into a valid DNS label (RFC 1123): lowercase,
// runs of invalid characters replaced by dashes, no leading or trailing
// dashes and at most 63 characters.
func sanitizeHostname(s string) string {
	s = invalidHostnameChars.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	return s
}

// parsePCIAddress parses a PCI address of the form <domain>:<bus>:<slot>.<function>
// (e.g. "0000:3b:00.1"), all components hexadecimal, into a HostDevice for PCI
// pass-through.
func parsePCIAddress(pciAddress string) (*providernetworkinterface.HostDevice, error) {
	parts := strings.FieldsFunc(pciAddress, func(r rune) bool { return r == ':' || r == '.' })
	if len(parts) != 4 {
		return nil, fmt.Errorf("expected address in format <domain>:<bus>:<slot>.<function>")
	}

	components := make([]uint, len(parts))
	for i, part := range parts {
		component, err := strconv.ParseUint(part, 16, 32)
		if err != nil {
			return nil, fmt.Errorf("error parsing component %q: %w", part, err)
		}
		components[i] = uint(component)
	}

	return &providernetworkinterface.HostDevice{
		Domain:   components[0],
		Bus:      components[1],
		Slot:     components[2],
		Function: components[3],
	}, nil
}

func (p *Plugin) Delete(ctx context.Context, computeNicName string, machineID string) error {
	name := AttachmentName(machineID, computeNicName)
	if err := p.run(ctx, commandDel, request{Name: name}, nil); err != nil {
		return err
	}
	return os.RemoveAll(p.host.MachineNetworkInterfaceDir(machineID, computeNicName))
}

// run executes the INI plugin binary once: INI_COMMAND=<command> in the
// environment, req as JSON on stdin. If out is non-nil, stdout is decoded into
// it as JSON (for ADD); otherwise stdout is ignored (for DEL).
func (p *Plugin) run(ctx context.Context, command string, req request, out any) error {
	input, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("error encoding ini plugin request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, p.pluginPath)
	cmd.Env = append(os.Environ(), envCommand+"="+command)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Per spec, a failing plugin SHOULD print {"error": "<message>"} to stdout.
		errResult := &errorResult{}
		if jsonErr := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), errResult); jsonErr == nil && errResult.Error != "" {
			return fmt.Errorf("ini plugin %s for attachment %q failed: %s", command, req.Name, errResult.Error)
		}
		if stderrMsg := strings.TrimSpace(stderr.String()); stderrMsg != "" {
			return fmt.Errorf("ini plugin %s for attachment %q failed: %w (stderr: %s)", command, req.Name, err, stderrMsg)
		}
		return fmt.Errorf("ini plugin %s for attachment %q failed: %w", command, req.Name, err)
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), out); err != nil {
		return fmt.Errorf("error decoding ini plugin %s result for attachment %q (stdout: %q): %w",
			command, req.Name, strings.TrimSpace(stdout.String()), err)
	}
	return nil
}

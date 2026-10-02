// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ini_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	machinepoolletv1alpha1 "github.com/ironcore-dev/ironcore/poollet/machinepoollet/api/v1alpha1"
	"github.com/ironcore-dev/libvirt-provider/api"
	providerhost "github.com/ironcore-dev/libvirt-provider/internal/host"
	providernetworkinterface "github.com/ironcore-dev/libvirt-provider/internal/plugins/networkinterface"
	"github.com/ironcore-dev/libvirt-provider/internal/plugins/networkinterface/ini"
	apiutils "github.com/ironcore-dev/provider-utils/apiutils/api"
)

const (
	testMachineID = "a3f9b2c1-4d5e-6f70-8192-a3b4c5d6e7f8"
	testNicName   = "nic0"
)

// writeFakePlugin writes a fake INI plugin executable. The script captures
// INI_COMMAND and stdin into the returned directory and executes body
// afterwards.
func writeFakePlugin(t *testing.T, body string) (pluginPath, captureDir string) {
	t.Helper()

	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
echo "$INI_COMMAND" >%q
cat >%q
%s
`, filepath.Join(dir, "command"), filepath.Join(dir, "stdin"), body)

	pluginPath = filepath.Join(dir, "inisix")
	if err := os.WriteFile(pluginPath, []byte(script), 0o755); err != nil {
		t.Fatalf("error writing fake plugin: %v", err)
	}
	return pluginPath, dir
}

func readCapture(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("error reading captured %s: %v", name, err)
	}
	return strings.TrimSpace(string(data))
}

func TestAttachmentName(t *testing.T) {
	nameRegexp := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,13}[a-z0-9])?$`)

	name := ini.AttachmentName(testMachineID, testNicName)
	if len(name) > 15 {
		t.Errorf("attachment name %q exceeds 15 characters", name)
	}
	if !nameRegexp.MatchString(name) {
		t.Errorf("attachment name %q does not match the INI name regex", name)
	}
	if again := ini.AttachmentName(testMachineID, testNicName); again != name {
		t.Errorf("attachment name is not stable: %q != %q", again, name)
	}
	if other := ini.AttachmentName(testMachineID, "nic1"); other == name {
		t.Errorf("attachment name does not differ across network interfaces: %q", other)
	}
	if other := ini.AttachmentName("b4f9b2c1-4d5e-6f70-8192-a3b4c5d6e7f8", testNicName); other == name {
		t.Errorf("attachment name does not differ across machines: %q", other)
	}
}

func TestApplyReturnsEthernetDeviceAndCreatesNicDir(t *testing.T) {
	pluginPath, captureDir := writeFakePlugin(t, `printf '{"tap": {"name": "inisix-tap0"}, "prefixes": ["2001:db8:4000:100::/64"], "ips": ["2001:db8:4000:100::1"]}\n'`)

	host, err := providerhost.NewLibvirtAt(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("error creating host: %v", err)
	}

	p := ini.NewPlugin(pluginPath)
	if err := p.Init(context.Background(), host); err != nil {
		t.Fatalf("error initializing plugin: %v", err)
	}

	machine := &api.Machine{Metadata: apiutils.Metadata{ID: testMachineID}}
	nic, err := p.Apply(context.Background(), &api.NetworkInterfaceSpec{Name: testNicName}, machine)
	if err != nil {
		t.Fatalf("error applying network interface: %v", err)
	}

	wantName := ini.AttachmentName(testMachineID, testNicName)
	if nic.Handle != wantName {
		t.Errorf("unexpected handle: got %q, want %q", nic.Handle, wantName)
	}
	if nic.Ethernet == nil {
		t.Fatalf("expected an ethernet network interface, got %#v", nic)
	}
	if nic.Ethernet.Dev != "inisix-tap0" {
		t.Errorf("unexpected tap device: got %q, want %q", nic.Ethernet.Dev, "inisix-tap0")
	}
	if len(nic.Prefixes) != 1 || nic.Prefixes[0] != "2001:db8:4000:100::/64" {
		t.Errorf("unexpected prefixes: got %v, want %v", nic.Prefixes, []string{"2001:db8:4000:100::/64"})
	}
	if len(nic.Ips) != 1 || nic.Ips[0] != "2001:db8:4000:100::1" {
		t.Errorf("unexpected ips: got %v, want %v", nic.Ips, []string{"2001:db8:4000:100::1"})
	}
	if nic.HostDevice != nil || nic.Direct != nil || nic.Isolated != nil || nic.ProviderNetwork != nil {
		t.Errorf("expected only the ethernet variant to be set, got %#v", nic)
	}

	if got := readCapture(t, captureDir, "command"); got != "ADD" {
		t.Errorf("unexpected INI_COMMAND: got %q, want %q", got, "ADD")
	}
	if got, want := readCapture(t, captureDir, "stdin"), fmt.Sprintf(`{"name":%q}`, wantName); got != want {
		t.Errorf("unexpected plugin stdin: got %s, want %s", got, want)
	}

	if _, err := os.Stat(host.MachineNetworkInterfaceDir(testMachineID, testNicName)); err != nil {
		t.Errorf("expected machine network interface dir to exist: %v", err)
	}
}

func TestApplyReturnsHostDeviceForVF(t *testing.T) {
	pluginPath, captureDir := writeFakePlugin(t, `printf '{"vf": {"pci-address": "0000:3b:00.1"}, "prefixes": ["2001:db8:4000:100::/64"]}\n'`)

	host, err := providerhost.NewLibvirtAt(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("error creating host: %v", err)
	}

	p := ini.NewPlugin(pluginPath)
	if err := p.Init(context.Background(), host); err != nil {
		t.Fatalf("error initializing plugin: %v", err)
	}

	machine := &api.Machine{Metadata: apiutils.Metadata{ID: testMachineID}}
	nic, err := p.Apply(context.Background(), &api.NetworkInterfaceSpec{Name: testNicName}, machine)
	if err != nil {
		t.Fatalf("error applying network interface: %v", err)
	}

	wantName := ini.AttachmentName(testMachineID, testNicName)
	if nic.Handle != wantName {
		t.Errorf("unexpected handle: got %q, want %q", nic.Handle, wantName)
	}
	if nic.HostDevice == nil {
		t.Fatalf("expected a host device network interface, got %#v", nic)
	}
	if *nic.HostDevice != (providernetworkinterface.HostDevice{Domain: 0x0000, Bus: 0x3b, Slot: 0x00, Function: 0x1}) {
		t.Errorf("unexpected host device: got %#v, want pci address 0000:3b:00.1", nic.HostDevice)
	}
	if len(nic.Prefixes) != 1 || nic.Prefixes[0] != "2001:db8:4000:100::/64" {
		t.Errorf("unexpected prefixes: got %v, want %v", nic.Prefixes, []string{"2001:db8:4000:100::/64"})
	}
	if len(nic.Ips) != 0 {
		t.Errorf("expected no ips, got ips=%v", nic.Ips)
	}
	if nic.Ethernet != nil || nic.Direct != nil || nic.Isolated != nil || nic.ProviderNetwork != nil {
		t.Errorf("expected only the host device variant to be set, got %#v", nic)
	}

	if got := readCapture(t, captureDir, "command"); got != "ADD" {
		t.Errorf("unexpected INI_COMMAND: got %q, want %q", got, "ADD")
	}
	if got, want := readCapture(t, captureDir, "stdin"), fmt.Sprintf(`{"name":%q}`, wantName); got != want {
		t.Errorf("unexpected plugin stdin: got %s, want %s", got, want)
	}

	if _, err := os.Stat(host.MachineNetworkInterfaceDir(testMachineID, testNicName)); err != nil {
		t.Errorf("expected machine network interface dir to exist: %v", err)
	}
}

func TestApplySendsHostname(t *testing.T) {
	wantName := ini.AttachmentName(testMachineID, testNicName)

	machine := func(hostname string, machineName string) *api.Machine {
		m := &api.Machine{Metadata: apiutils.Metadata{ID: testMachineID}}
		if hostname != "" {
			m.Spec.GuestConfig = &api.MachineGuestConfig{HostName: hostname}
		}
		if machineName != "" {
			m.Metadata.Labels = map[string]string{machinepoolletv1alpha1.MachineNameLabel: machineName}
		}
		return m
	}

	tests := map[string]struct {
		machine      *api.Machine
		wantHostname string
	}{
		"explicit guest hostname": {
			machine:      machine("explicit-host", ""),
			wantHostname: "explicit-host",
		},
		"explicit guest hostname wins over machine name": {
			machine:      machine("explicit-host", "object-name"),
			wantHostname: "explicit-host",
		},
		"machine name from label": {
			machine:      machine("", "object-name"),
			wantHostname: "object-name",
		},
		"machine name is sanitized to a DNS label": {
			machine:      machine("", "My.Machine_Example"),
			wantHostname: "my-machine-example",
		},
		"machine name is truncated to 63 characters": {
			machine:      machine("", strings.Repeat("a", 80)),
			wantHostname: strings.Repeat("a", 63),
		},
		"truncation at an invalid character leaves no trailing dash": {
			machine:      machine("", strings.Repeat("a", 62)+".bb"),
			wantHostname: strings.Repeat("a", 62),
		},
		"no hostname derivable": {
			machine:      machine("", ""),
			wantHostname: "",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pluginPath, captureDir := writeFakePlugin(t, `printf '{"tap": {"name": "inisix-tap0"}, "prefixes": ["2001:db8:4000:100::/64"]}\n'`)

			host, err := providerhost.NewLibvirtAt(t.TempDir(), nil)
			if err != nil {
				t.Fatalf("error creating host: %v", err)
			}

			p := ini.NewPlugin(pluginPath)
			if err := p.Init(context.Background(), host); err != nil {
				t.Fatalf("error initializing plugin: %v", err)
			}

			if _, err := p.Apply(context.Background(), &api.NetworkInterfaceSpec{Name: testNicName}, tt.machine); err != nil {
				t.Fatalf("error applying network interface: %v", err)
			}

			wantStdin := fmt.Sprintf(`{"name":%q}`, wantName)
			if tt.wantHostname != "" {
				wantStdin = fmt.Sprintf(`{"name":%q,"hostname":%q}`, wantName, tt.wantHostname)
			}
			if got := readCapture(t, captureDir, "stdin"); got != wantStdin {
				t.Errorf("unexpected plugin stdin: got %s, want %s", got, wantStdin)
			}
		})
	}
}

func TestDeleteRunsDELAndRemovesNicDir(t *testing.T) {
	pluginPath, captureDir := writeFakePlugin(t, `exit 0`)

	host, err := providerhost.NewLibvirtAt(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("error creating host: %v", err)
	}
	if err := os.MkdirAll(host.MachineNetworkInterfaceDir(testMachineID, testNicName), 0o777); err != nil {
		t.Fatalf("error creating machine network interface dir: %v", err)
	}

	p := ini.NewPlugin(pluginPath)
	if err := p.Init(context.Background(), host); err != nil {
		t.Fatalf("error initializing plugin: %v", err)
	}

	if err := p.Delete(context.Background(), testNicName, testMachineID); err != nil {
		t.Fatalf("error deleting network interface: %v", err)
	}

	wantName := ini.AttachmentName(testMachineID, testNicName)
	if got := readCapture(t, captureDir, "command"); got != "DEL" {
		t.Errorf("unexpected INI_COMMAND: got %q, want %q", got, "DEL")
	}
	if got, want := readCapture(t, captureDir, "stdin"), fmt.Sprintf(`{"name":%q}`, wantName); got != want {
		t.Errorf("unexpected plugin stdin: got %s, want %s", got, want)
	}

	if _, err := os.Stat(host.MachineNetworkInterfaceDir(testMachineID, testNicName)); !os.IsNotExist(err) {
		t.Errorf("expected machine network interface dir to be removed, stat error: %v", err)
	}

	if err := p.Delete(context.Background(), "nonexistent", testMachineID); err != nil {
		t.Errorf("expected DEL to be idempotent for unknown attachments, got: %v", err)
	}
}

func TestApplyPluginFailure(t *testing.T) {
	tests := map[string]struct {
		body        string
		wantErrPart string
	}{
		"json error on stdout": {
			body:        `printf '{"error": "no free prefix available"}\n'; exit 1`,
			wantErrPart: "no free prefix available",
		},
		"human readable error on stderr": {
			body:        `echo "tuntap: device busy" >&2; exit 1`,
			wantErrPart: "tuntap: device busy",
		},
		"unparsable result": {
			body:        `printf 'not-json\n'; exit 0`,
			wantErrPart: "error decoding",
		},
		"invalid prefix": {
			body:        `printf '{"tap": {"name": "inisix-tap0"}, "prefixes": ["not-a-prefix"]}\n'; exit 0`,
			wantErrPart: "invalid prefix",
		},
		"missing prefixes": {
			body:        `printf '{"tap": {"name": "inisix-tap0"}}\n'; exit 0`,
			wantErrPart: "no prefixes",
		},
		"empty prefixes": {
			body:        `printf '{"tap": {"name": "inisix-tap0"}, "prefixes": []}\n'; exit 0`,
			wantErrPart: "no prefixes",
		},
		"invalid ip": {
			body:        `printf '{"tap": {"name": "inisix-tap0"}, "prefixes": ["2001:db8:4000:100::/64"], "ips": ["2001:db8:4000:100::1/64"]}\n'; exit 0`,
			wantErrPart: "invalid ip",
		},
		"no device member": {
			body:        `printf '{"prefixes": ["2001:db8:4000:100::/64"]}\n'; exit 0`,
			wantErrPart: "no device member",
		},
		"unknown device member": {
			body:        `printf '{"foobar": {"name": "inisix-tap0"}, "prefixes": ["2001:db8:4000:100::/64"]}\n'; exit 0`,
			wantErrPart: "no device member",
		},
		"both device members": {
			body:        `printf '{"tap": {"name": "inisix-tap0"}, "vf": {"pci-address": "0000:3b:00.1"}, "prefixes": ["2001:db8:4000:100::/64"]}\n'; exit 0`,
			wantErrPart: "both tap and vf",
		},
		"tap device without name": {
			body:        `printf '{"tap": {"name": ""}, "prefixes": ["2001:db8:4000:100::/64"]}\n'; exit 0`,
			wantErrPart: "tap device without a name",
		},
		"invalid vf pci-address": {
			body:        `printf '{"vf": {"pci-address": "3b:00.1"}, "prefixes": ["2001:db8:4000:100::/64"]}\n'; exit 0`,
			wantErrPart: "invalid vf pci-address",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pluginPath, _ := writeFakePlugin(t, tt.body)

			host, err := providerhost.NewLibvirtAt(t.TempDir(), nil)
			if err != nil {
				t.Fatalf("error creating host: %v", err)
			}

			p := ini.NewPlugin(pluginPath)
			if err := p.Init(context.Background(), host); err != nil {
				t.Fatalf("error initializing plugin: %v", err)
			}

			machine := &api.Machine{Metadata: apiutils.Metadata{ID: testMachineID}}
			if _, err := p.Apply(context.Background(), &api.NetworkInterfaceSpec{Name: testNicName}, machine); err == nil {
				t.Fatal("expected apply to fail, got nil error")
			} else if !strings.Contains(err.Error(), tt.wantErrPart) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErrPart)
			}
		})
	}
}

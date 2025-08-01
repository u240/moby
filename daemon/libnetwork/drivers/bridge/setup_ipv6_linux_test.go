package bridge

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/nlwrap"
	"github.com/moby/moby/v2/internal/testutils/netnsutils"
	"github.com/vishvananda/netlink"
)

func TestSetupIPv6(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	nh, err := nlwrap.NewHandle()
	if err != nil {
		t.Fatal(err)
	}
	defer nh.Close()

	config, br := setupTestInterface(t, nh)
	addr, nw, _ := net.ParseCIDR("fdcc:949:6399:1234::1/64")
	config.AddressIPv6 = &net.IPNet{IP: addr, Mask: nw.Mask}
	if err := setupBridgeIPv6(config, br); err != nil {
		t.Fatalf("Failed to setup bridge IPv6: %v", err)
	}

	procSetting, err := os.ReadFile(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/disable_ipv6", config.BridgeName))
	if err != nil {
		t.Fatalf("Failed to read disable_ipv6 kernel setting: %v", err)
	}

	if expected := []byte("0\n"); !bytes.Equal(expected, procSetting) {
		t.Fatalf("Invalid kernel setting disable_ipv6: expected %q, got %q", string(expected), string(procSetting))
	}

	addrsv6, err := nh.AddrList(br.Link, netlink.FAMILY_V6)
	if err != nil {
		t.Fatalf("Failed to list device IPv6 addresses: %v", err)
	}

	var found bool
	for _, addr := range addrsv6 {
		if config.AddressIPv6.String() == addr.IPNet.String() {
			found = true
			break
		}
	}

	if !found {
		t.Fatalf("Bridge device does not have requested IPv6 address %v", config.AddressIPv6)
	}
}

func TestSetupGatewayIPv6(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	_, nw, _ := net.ParseCIDR("2001:db8:ea9:9abc:ffff::/80")
	gw := net.ParseIP("2001:db8:ea9:9abc:ffff::254")

	config := &networkConfiguration{
		BridgeName:         DefaultBridgeName,
		AddressIPv6:        nw,
		DefaultGatewayIPv6: gw,
	}

	nh, err := nlwrap.NewHandle()
	if err != nil {
		t.Fatal(err)
	}
	defer nh.Close()

	br := &bridgeInterface{nlh: nh}

	if err := setupGatewayIPv6(config, br); err != nil {
		t.Fatalf("Set Default Gateway failed: %v", err)
	}

	if !gw.Equal(br.gatewayIPv6) {
		t.Fatalf("Set Default Gateway failed. Expected %v, Found %v", gw, br.gatewayIPv6)
	}
}

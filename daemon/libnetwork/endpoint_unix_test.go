//go:build !windows

package libnetwork

import (
	"context"
	"os"
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/ipams/defaultipam"
	"github.com/moby/moby/v2/internal/testutils/netnsutils"
)

func TestHostsEntries(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	expectedHostsFile := `127.0.0.1	localhost
::1	localhost ip6-localhost ip6-loopback
fe00::	ip6-localnet
ff00::	ip6-mcastprefix
ff02::1	ip6-allnodes
ff02::2	ip6-allrouters
192.168.222.2	somehost.example.com somehost
fe90::2	somehost.example.com somehost
`

	opts := []NetworkOption{
		NetworkOptionEnableIPv4(true),
		NetworkOptionEnableIPv6(true),
		NetworkOptionIpam(defaultipam.DriverName, "",
			[]*IpamConf{{PreferredPool: "192.168.222.0/24", Gateway: "192.168.222.1"}},
			[]*IpamConf{{PreferredPool: "fe90::/64", Gateway: "fe90::1"}}, nil),
	}

	ctrlr, nws := getTestEnv(t, opts)

	hostsFile, err := os.CreateTemp("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(hostsFile.Name())

	sbx, err := ctrlr.NewSandbox(context.Background(), "sandbox1", OptionHostsPath(hostsFile.Name()), OptionHostname("somehost.example.com"))
	if err != nil {
		t.Fatal(err)
	}

	ep1, err := nws[0].CreateEndpoint(context.Background(), "ep1")
	if err != nil {
		t.Fatal(err)
	}

	if err := ep1.Join(context.Background(), sbx, JoinOptionPriority(1)); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(hostsFile.Name())
	if err != nil {
		t.Fatal(err)
	}

	if string(data) != expectedHostsFile {
		t.Fatalf("expected the hosts file to read:\n%q\nbut instead got the following:\n%q\n", expectedHostsFile, string(data))
	}

	if err := sbx.Delete(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(ctrlr.sandboxes) != 0 {
		t.Fatalf("controller sandboxes is not empty. len = %d", len(ctrlr.sandboxes))
	}
}

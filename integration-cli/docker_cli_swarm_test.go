//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/cfssl/helpers"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/versions"
	"github.com/moby/moby/v2/daemon/libnetwork/driverapi"
	"github.com/moby/moby/v2/daemon/libnetwork/ipamapi"
	remoteipam "github.com/moby/moby/v2/daemon/libnetwork/ipams/remote/api"
	"github.com/moby/moby/v2/daemon/libnetwork/nlwrap"
	"github.com/moby/moby/v2/integration-cli/checker"
	"github.com/moby/moby/v2/integration-cli/cli"
	"github.com/moby/moby/v2/integration-cli/daemon"
	"github.com/moby/moby/v2/pkg/plugins"
	"github.com/moby/moby/v2/testutil"
	testdaemon "github.com/moby/moby/v2/testutil/daemon"
	"github.com/moby/swarmkit/v2/ca/keyutils"
	"github.com/vishvananda/netlink"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/fs"
	"gotest.tools/v3/icmd"
	"gotest.tools/v3/poll"
)

func (s *DockerSwarmSuite) TestSwarmUpdate(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	getSpec := func() swarm.Spec {
		sw := d.GetSwarm(c)
		return sw.Spec
	}

	out, err := d.Cmd("swarm", "update", "--cert-expiry", "30h", "--dispatcher-heartbeat", "11s")
	assert.NilError(c, err, out)

	spec := getSpec()
	assert.Check(c, is.Equal(spec.CAConfig.NodeCertExpiry, 30*time.Hour))
	assert.Check(c, is.Equal(spec.Dispatcher.HeartbeatPeriod, 11*time.Second))

	// setting anything under 30m for cert-expiry is not allowed
	out, err = d.Cmd("swarm", "update", "--cert-expiry", "15m")
	assert.ErrorContains(c, err, "")
	assert.Assert(c, is.Contains(out, "minimum certificate expiry time"))
	spec = getSpec()
	assert.Check(c, is.Equal(spec.CAConfig.NodeCertExpiry, 30*time.Hour))

	// passing an external CA (this is without starting a root rotation) does not fail
	cli.Docker(cli.Args("swarm", "update", "--external-ca", "protocol=cfssl,url=https://something.org",
		"--external-ca", "protocol=cfssl,url=https://somethingelse.org,cacert=fixtures/https/ca.pem"),
		cli.Daemon(d)).Assert(c, icmd.Success)

	expected, err := os.ReadFile("fixtures/https/ca.pem")
	assert.NilError(c, err)

	version := cli.Docker(cli.Args("version", "--format", "{{ .Client.Version }}"), cli.Daemon(d)).Stdout()
	version = strings.TrimSpace(version)
	// This was broken in v18.06
	// See: https://github.com/docker/cli/pull/5995
	if version != "" && versions.LessThan(version, "18.06") {
		spec = getSpec()
		sw := d.GetSwarm(c)
		if assert.Check(c, is.Len(spec.CAConfig.ExternalCAs, 2)) {
			assert.Check(c, is.Equal(spec.CAConfig.ExternalCAs[0].CACert, sw.TLSInfo.TrustRoot))
			assert.Check(c, is.Equal(spec.CAConfig.ExternalCAs[1].CACert, string(expected)))
		}
	}

	// passing an invalid external CA fails
	tempFile := fs.NewFile(c, "testfile", fs.WithContent("fakecert"))

	result := cli.Docker(cli.Args("swarm", "update",
		"--external-ca", fmt.Sprintf("protocol=cfssl,url=https://something.org,cacert=%s", tempFile.Path())),
		cli.Daemon(d))
	result.Assert(c, icmd.Expected{
		ExitCode: 125,
		Err:      "must be in PEM format",
	})
}

func (s *DockerSwarmSuite) TestSwarmInit(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, false, false)

	getSpec := func() swarm.Spec {
		sw := d.GetSwarm(c)
		return sw.Spec
	}

	// passing an invalid external CA fails
	tempFile := fs.NewFile(c, "testfile", fs.WithContent("fakecert"))

	result := cli.Docker(cli.Args("swarm", "init", "--cert-expiry", "30h", "--dispatcher-heartbeat", "11s",
		"--external-ca", fmt.Sprintf("protocol=cfssl,url=https://somethingelse.org,cacert=%s", tempFile.Path())),
		cli.Daemon(d))
	result.Assert(c, icmd.Expected{
		ExitCode: 125,
		Err:      "must be in PEM format",
	})

	cli.Docker(cli.Args("swarm", "init", "--cert-expiry", "30h", "--dispatcher-heartbeat", "11s",
		"--external-ca", "protocol=cfssl,url=https://something.org",
		"--external-ca", "protocol=cfssl,url=https://somethingelse.org,cacert=fixtures/https/ca.pem"),
		cli.Daemon(d)).Assert(c, icmd.Success)

	expected, err := os.ReadFile("fixtures/https/ca.pem")
	assert.NilError(c, err)

	spec := getSpec()
	assert.Check(c, is.Equal(spec.CAConfig.NodeCertExpiry, 30*time.Hour))
	assert.Check(c, is.Equal(spec.Dispatcher.HeartbeatPeriod, 11*time.Second))

	version := cli.Docker(cli.Args("version", "--format", "{{ .Client.Version }}"), cli.Daemon(d)).Stdout()
	version = strings.TrimSpace(version)
	// This was broken in v18.06
	// See: https://github.com/docker/cli/pull/5995
	if version != "" && versions.LessThan(version, "18.06") {
		if assert.Check(c, is.Len(spec.CAConfig.ExternalCAs, 2)) {
			// TODO: Should this actually be:
			// assert.Check(c, is.Equal(spec.CAConfig.ExternalCAs[0].CACert, sw.TLSInfo.TrustRoot))
			assert.Check(c, is.Equal(spec.CAConfig.ExternalCAs[0].CACert, ""))

			assert.Check(c, is.Equal(spec.CAConfig.ExternalCAs[1].CACert, string(expected)))
		}
	}

	assert.NilError(c, d.SwarmLeave(ctx, c, true))
	cli.Docker(cli.Args("swarm", "init"), cli.Daemon(d)).Assert(c, icmd.Success)

	spec = getSpec()
	assert.Check(c, is.Equal(spec.CAConfig.NodeCertExpiry, 90*24*time.Hour))
	assert.Check(c, is.Equal(spec.Dispatcher.HeartbeatPeriod, 5*time.Second))
}

func (s *DockerSwarmSuite) TestSwarmInitIPv6(c *testing.T) {
	ctx := testutil.GetContext(c)
	d1 := s.AddDaemon(ctx, c, false, false)
	cli.Docker(cli.Args("swarm", "init", "--listen-addr", "::1"),
		cli.Daemon(d1)).Assert(c, icmd.Success)

	token := s.daemons[0].JoinTokens(c).Worker
	d2 := s.AddDaemon(ctx, c, false, false)
	cli.Docker(cli.Args("swarm", "join", "[::1]", "--token", token),
		cli.Daemon(d2)).Assert(c, icmd.Success)

	out := cli.Docker(cli.Args("info"), cli.Daemon(d2)).Assert(c, icmd.Success).Combined()
	assert.Assert(c, is.Contains(out, "Swarm: active"))
}

func (s *DockerSwarmSuite) TestSwarmInitUnspecifiedAdvertiseAddr(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, false, false)
	out, err := d.Cmd("swarm", "init", "--advertise-addr", "0.0.0.0")
	assert.ErrorContains(c, err, "")
	assert.Assert(c, is.Contains(out, "advertise address must be a non-zero IP address"))
}

func (s *DockerSwarmSuite) TestSwarmIncompatibleDaemon(c *testing.T) {
	ctx := testutil.GetContext(c)
	// init swarm mode and stop a daemon
	d := s.AddDaemon(ctx, c, true, true)
	info := d.SwarmInfo(ctx, c)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateActive)
	d.Stop(c)

	// start a daemon with --live-restore
	err := d.StartWithError("--live-restore")
	assert.ErrorContains(c, err, "")
	content, err := d.ReadLogFile()
	assert.NilError(c, err)
	assert.Assert(c, is.Contains(string(content), "--live-restore daemon configuration is incompatible with swarm mode"))
	// restart for teardown
	d.StartNode(c)
}

func (s *DockerSwarmSuite) TestSwarmServiceTemplatingHostname(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)
	hostname, err := d.Cmd("node", "inspect", "--format", "{{.Description.Hostname}}", "self")
	assert.NilError(c, err, hostname)

	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", "test", "--hostname", "{{.Service.Name}}-{{.Task.Slot}}-{{.Node.Hostname}}", "busybox", "top")
	assert.NilError(c, err, out)

	// make sure task has been deployed.
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))

	containers := d.ActiveContainers(testutil.GetContext(c), c)
	out, err = d.Cmd("inspect", "--type", "container", "--format", "{{.Config.Hostname}}", containers[0])
	assert.NilError(c, err, out)
	assert.Equal(c, strings.Split(out, "\n")[0], "test-1-"+strings.Split(hostname, "\n")[0], "hostname with templating invalid")
}

// Test case for #24270
func (s *DockerSwarmSuite) TestSwarmServiceListFilter(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	name1 := "redis-cluster-md5"
	name2 := "redis-cluster"
	name3 := "other-cluster"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name1, "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	out, err = d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name2, "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	out, err = d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name3, "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	filter1 := "name=redis-cluster-md5"
	filter2 := "name=redis-cluster"

	// We search checker.Contains with `name+" "` to prevent prefix only.
	out, err = d.Cmd("service", "ls", "--filter", filter1)
	assert.NilError(c, err, out)
	assert.Check(c, is.Contains(out, name1+" "))
	assert.Check(c, !strings.Contains(out, name2+" "), out)
	assert.Check(c, !strings.Contains(out, name3+" "), out)
	out, err = d.Cmd("service", "ls", "--filter", filter2)
	assert.NilError(c, err, out)
	assert.Check(c, is.Contains(out, name1+" "))
	assert.Check(c, is.Contains(out, name2+" "))
	assert.Check(c, !strings.Contains(out, name3+" "))
	out, err = d.Cmd("service", "ls")
	assert.NilError(c, err, out)
	assert.Check(c, is.Contains(out, name1+" "))
	assert.Check(c, is.Contains(out, name2+" "))
	assert.Check(c, is.Contains(out, name3+" "))
}

func (s *DockerSwarmSuite) TestSwarmNodeListFilter(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	out, err := d.Cmd("node", "inspect", "--format", "{{ .Description.Hostname }}", "self")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")
	name := strings.TrimSpace(out)

	filter := "name=" + name[:4]

	out, err = d.Cmd("node", "ls", "--filter", filter)
	assert.NilError(c, err, out)
	assert.Assert(c, is.Contains(out, name))
	out, err = d.Cmd("node", "ls", "--filter", "name=none")
	assert.NilError(c, err, out)
	assert.Assert(c, !strings.Contains(out, name), out)
}

func (s *DockerSwarmSuite) TestSwarmNodeTaskListFilter(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	name := "redis-cluster-md5"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "--replicas=3", "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	// make sure task has been deployed.
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(3)), poll.WithTimeout(defaultReconciliationTimeout))

	filter := "name=redis-cluster"

	out, err = d.Cmd("node", "ps", "--filter", filter, "self")
	assert.NilError(c, err, out)
	assert.Check(c, is.Contains(out, name+".1"))
	assert.Check(c, is.Contains(out, name+".2"))
	assert.Check(c, is.Contains(out, name+".3"))
	out, err = d.Cmd("node", "ps", "--filter", "name=none", "self")
	assert.NilError(c, err, out)
	assert.Assert(c, !strings.Contains(out, name+".1"), out)
	assert.Assert(c, !strings.Contains(out, name+".2"), out)
	assert.Assert(c, !strings.Contains(out, name+".3"), out)
}

// Test case for #25375
func (s *DockerSwarmSuite) TestSwarmPublishAdd(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	name := "top"
	// this first command does not have to be retried because service creates
	// don't return out of sequence errors.
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "--label", "x=y", "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	out, err = d.CmdRetryOutOfSequence("service", "update", "--detach", "--publish-add", "80:80", name)
	assert.NilError(c, err, out)

	out, err = d.CmdRetryOutOfSequence("service", "update", "--detach", "--publish-add", "80:80", name)
	assert.NilError(c, err, out)

	_, err = d.CmdRetryOutOfSequence("service", "update", "--detach", "--publish-add", "80:80", "--publish-add", "80:20", name)
	assert.Check(c, is.ErrorContains(err, ""))

	// this last command does not have to be retried because service inspect
	// does not return out of sequence errors.
	out, err = d.Cmd("service", "inspect", "--format", "{{ .Spec.EndpointSpec.Ports }}", name)
	assert.NilError(c, err, out)
	assert.Check(c, is.Equal(strings.TrimSpace(out), "[{ tcp 80 80 ingress}]"))
}

func (s *DockerSwarmSuite) TestSwarmServiceWithGroup(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	name := "top"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "--user", "root:root", "--group", "wheel", "--group", "audio", "--group", "staff", "--group", "777", "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	// make sure task has been deployed.
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))

	out, err = d.Cmd("ps", "-q")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	container := strings.TrimSpace(out)

	out, err = d.Cmd("exec", container, "id")
	assert.NilError(c, err, out)
	assert.Check(c, is.Equal(strings.TrimSpace(out), "uid=0(root) gid=0(root) groups=0(root),10(wheel),29(audio),50(staff),777"))
}

func (s *DockerSwarmSuite) TestSwarmContainerAutoStart(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	out, err := d.Cmd("network", "create", "--attachable", "-d", "overlay", "foo")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	out, err = d.Cmd("run", "-id", "--restart=always", "--net=foo", "--name=test", "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	out, err = d.Cmd("ps", "-q")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	d.RestartNode(c)

	out, err = d.Cmd("ps", "-q")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")
}

func (s *DockerSwarmSuite) TestSwarmContainerEndpointOptions(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	out, err := d.Cmd("network", "create", "--attachable", "-d", "overlay", "foo")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	out, err = d.Cmd("run", "-d", "--net=foo", "--name=first", "--net-alias=first-alias", "busybox:glibc", "top")
	assert.NilError(c, err, out)

	out, err = d.Cmd("run", "-d", "--net=foo", "--name=second", "busybox:glibc", "top")
	assert.NilError(c, err, out)

	out, err = d.Cmd("run", "-d", "--net=foo", "--net-alias=third-alias", "busybox:glibc", "top")
	assert.NilError(c, err, out)

	// ping first container and its alias, also ping third and anonymous container by its alias
	out, err = d.Cmd("exec", "second", "ping", "-c", "1", "first")
	assert.NilError(c, err, out)
	out, err = d.Cmd("exec", "second", "ping", "-c", "1", "first-alias")
	assert.NilError(c, err, out)
	out, err = d.Cmd("exec", "second", "ping", "-c", "1", "third-alias")
	assert.NilError(c, err, out)
}

func (s *DockerSwarmSuite) TestSwarmContainerAttachByNetworkId(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	out, err := d.Cmd("network", "create", "--attachable", "-d", "overlay", "testnet")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")
	networkID := strings.TrimSpace(out)

	out, err = d.Cmd("run", "-d", "--net", networkID, "busybox", "top")
	assert.NilError(c, err, out)
	cID := strings.TrimSpace(out)
	err = d.WaitRun(cID)
	assert.NilError(c, err)

	out, err = d.Cmd("rm", "-f", cID)
	assert.NilError(c, err, out)

	out, err = d.Cmd("network", "rm", "testnet")
	assert.NilError(c, err, out)

	checkNetwork := func(*testing.T) (interface{}, string) {
		out, err := d.Cmd("network", "ls")
		assert.NilError(c, err)
		return out, ""
	}

	poll.WaitOn(c, pollCheck(c, checkNetwork, checker.Not(checker.Contains("testnet"))), poll.WithTimeout(3*time.Second))
}

func (s *DockerSwarmSuite) TestOverlayAttachable(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	out, err := d.Cmd("network", "create", "-d", "overlay", "--attachable", "ovnet")
	assert.NilError(c, err, out)

	// validate attachable
	out, err = d.Cmd("network", "inspect", "--format", "{{json .Attachable}}", "ovnet")
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "true")

	// validate containers can attach to this overlay network
	out, err = d.Cmd("run", "-d", "--network", "ovnet", "--name", "c1", "busybox", "top")
	assert.NilError(c, err, out)

	// redo validation, there was a bug that the value of attachable changes after
	// containers attach to the network
	out, err = d.Cmd("network", "inspect", "--format", "{{json .Attachable}}", "ovnet")
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "true")
}

func (s *DockerSwarmSuite) TestOverlayAttachableOnSwarmLeave(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	// Create an attachable swarm network
	nwName := "attovl"
	out, err := d.Cmd("network", "create", "-d", "overlay", "--attachable", nwName)
	assert.NilError(c, err, out)

	// Connect a container to the network
	out, err = d.Cmd("run", "-d", "--network", nwName, "--name", "c1", "busybox", "top")
	assert.NilError(c, err, out)

	// Leave the swarm
	assert.NilError(c, d.SwarmLeave(ctx, c, true))

	// Check the container is disconnected
	out, err = d.Cmd("inspect", "c1", "--format", "{{.NetworkSettings.Networks."+nwName+"}}")
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "<no value>")

	// Check the network is gone
	out, err = d.Cmd("network", "ls", "--format", "{{.Name}}")
	assert.NilError(c, err, out)
	assert.Assert(c, !strings.Contains(out, nwName), out)
}

func (s *DockerSwarmSuite) TestOverlayAttachableReleaseResourcesOnFailure(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	// Create attachable network
	out, err := d.Cmd("network", "create", "-d", "overlay", "--attachable", "--subnet", "10.10.9.0/24", "ovnet")
	assert.NilError(c, err, out)

	// Attach a container with specific IP
	out, err = d.Cmd("run", "-d", "--network", "ovnet", "--name", "c1", "--ip", "10.10.9.33", "busybox", "top")
	assert.NilError(c, err, out)

	// Attempt to attach another container with same IP, must fail
	out, err = d.Cmd("run", "-d", "--network", "ovnet", "--name", "c2", "--ip", "10.10.9.33", "busybox", "top")
	assert.ErrorContains(c, err, "", out)

	// Remove first container
	out, err = d.Cmd("rm", "-f", "c1")
	assert.NilError(c, err, out)

	// Verify the network can be removed, no phantom network attachment task left over
	out, err = d.Cmd("network", "rm", "ovnet")
	assert.NilError(c, err, out)
}

func (s *DockerSwarmSuite) TestSwarmIngressNetwork(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	// Ingress network can be removed
	removeNetwork := func(name string) *icmd.Result {
		return cli.Docker(
			cli.Args("-H", d.Sock(), "network", "rm", name),
			cli.WithStdin(strings.NewReader("Y")))
	}

	result := removeNetwork("ingress")
	result.Assert(c, icmd.Success)

	// And recreated
	out, err := d.Cmd("network", "create", "-d", "overlay", "--ingress", "new-ingress")
	assert.NilError(c, err, out)

	// But only one is allowed
	out, err = d.Cmd("network", "create", "-d", "overlay", "--ingress", "another-ingress")
	assert.ErrorContains(c, err, "")
	assert.Assert(c, strings.Contains(strings.TrimSpace(out), "is already present"), out)
	// It cannot be removed if it is being used
	out, err = d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", "srv1", "-p", "9000:8000", "busybox", "top")
	assert.NilError(c, err, out)

	result = removeNetwork("new-ingress")
	result.Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "ingress network cannot be removed because service",
	})

	// But it can be removed once no more services depend on it
	out, err = d.Cmd("service", "update", "--detach", "--publish-rm", "9000:8000", "srv1")
	assert.NilError(c, err, out)

	result = removeNetwork("new-ingress")
	result.Assert(c, icmd.Success)

	// A service which needs the ingress network cannot be created if no ingress is present
	out, err = d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", "srv2", "-p", "500:500", "busybox", "top")
	assert.ErrorContains(c, err, "")
	assert.Assert(c, strings.Contains(strings.TrimSpace(out), "no ingress network is present"), out)
	// An existing service cannot be updated to use the ingress nw if the nw is not present
	out, err = d.Cmd("service", "update", "--detach", "--publish-add", "9000:8000", "srv1")
	assert.ErrorContains(c, err, "")
	assert.Assert(c, strings.Contains(strings.TrimSpace(out), "no ingress network is present"), out)
	// But services which do not need routing mesh can be created regardless
	out, err = d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", "srv3", "--endpoint-mode", "dnsrr", "busybox", "top")
	assert.NilError(c, err, out)
}

func (s *DockerSwarmSuite) TestSwarmCreateServiceWithNoIngressNetwork(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	// Remove ingress network
	result := cli.Docker(
		cli.Args("-H", d.Sock(), "network", "rm", "ingress"),
		cli.WithStdin(strings.NewReader("Y")))
	result.Assert(c, icmd.Success)

	// Create a overlay network and launch a service on it
	// Make sure nothing panics because ingress network is missing
	out, err := d.Cmd("network", "create", "-d", "overlay", "another-network")
	assert.NilError(c, err, out)
	out, err = d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", "srv4", "--network", "another-network", "busybox", "top")
	assert.NilError(c, err, out)
}

// Test case for #24108, also the case from:
// https://github.com/moby/moby/pull/24620#issuecomment-233715656
func (s *DockerSwarmSuite) TestSwarmTaskListFilter(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	name := "redis-cluster-md5"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "--replicas=3", "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	filter := "name=redis-cluster"

	checkNumTasks := func(*testing.T) (interface{}, string) {
		out, err := d.Cmd("service", "ps", "--filter", filter, name)
		assert.NilError(c, err, out)
		return len(strings.Split(out, "\n")) - 2, "" // includes header and nl in last line
	}

	// wait until all tasks have been created
	poll.WaitOn(c, pollCheck(c, checkNumTasks, checker.Equals(3)), poll.WithTimeout(defaultReconciliationTimeout))

	out, err = d.Cmd("service", "ps", "--filter", filter, name)
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(out, name+".1"), out)
	assert.Assert(c, strings.Contains(out, name+".2"), out)
	assert.Assert(c, strings.Contains(out, name+".3"), out)
	out, err = d.Cmd("service", "ps", "--filter", "name="+name+".1", name)
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(out, name+".1"), out)
	assert.Assert(c, !strings.Contains(out, name+".2"), out)
	assert.Assert(c, !strings.Contains(out, name+".3"), out)
	out, err = d.Cmd("service", "ps", "--filter", "name=none", name)
	assert.NilError(c, err, out)
	assert.Assert(c, !strings.Contains(out, name+".1"), out)
	assert.Assert(c, !strings.Contains(out, name+".2"), out)
	assert.Assert(c, !strings.Contains(out, name+".3"), out)
	name = "redis-cluster-sha1"
	out, err = d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "--mode=global", "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	poll.WaitOn(c, pollCheck(c, checkNumTasks, checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))

	filter = "name=redis-cluster"
	out, err = d.Cmd("service", "ps", "--filter", filter, name)
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(out, name), out)
	out, err = d.Cmd("service", "ps", "--filter", "name="+name, name)
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(out, name), out)
	out, err = d.Cmd("service", "ps", "--filter", "name=none", name)
	assert.NilError(c, err, out)
	assert.Assert(c, !strings.Contains(out, name), out)
}

func (s *DockerSwarmSuite) TestPsListContainersFilterIsTask(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	// Create a bare container
	out, err := d.Cmd("run", "-d", "--name=bare-container", "busybox", "top")
	assert.NilError(c, err, out)
	bareID := strings.TrimSpace(out)[:12]
	// Create a service
	name := "busybox-top"
	out, err = d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	// make sure task has been deployed.
	poll.WaitOn(c, pollCheck(c, d.CheckServiceRunningTasks(ctx, name), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))

	// Filter non-tasks
	out, err = d.Cmd("ps", "-a", "-q", "--filter=is-task=false")
	assert.NilError(c, err, out)
	psOut := strings.TrimSpace(out)
	assert.Equal(c, psOut, bareID, fmt.Sprintf("Expected id %s, got %s for is-task label, output %q", bareID, psOut, out))

	// Filter tasks
	out, err = d.Cmd("ps", "-a", "-q", "--filter=is-task=true")
	assert.NilError(c, err, out)
	lines := strings.Split(strings.Trim(out, "\n "), "\n")
	assert.Equal(c, len(lines), 1)
	assert.Assert(c, lines[0] != bareID, "Expected not %s, but got it for is-task label, output %q", bareID, out)
}

const (
	globalNetworkPlugin = "global-network-plugin"
	globalIPAMPlugin    = "global-ipam-plugin"
)

func setupRemoteGlobalNetworkPlugin(t *testing.T, mux *http.ServeMux, url, netDrv, ipamDrv string) {
	mux.HandleFunc("/Plugin.Activate", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", plugins.VersionMimetype)
		_, err := fmt.Fprintf(w, `{"Implements": ["%s", "%s"]}`, driverapi.NetworkPluginEndpointType, ipamapi.PluginEndpointType)
		assert.NilError(t, err)
	})

	// Network driver implementation
	mux.HandleFunc(fmt.Sprintf("/%s.GetCapabilities", driverapi.NetworkPluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", plugins.VersionMimetype)
		_, err := fmt.Fprint(w, `{"Scope":"global"}`)
		assert.NilError(t, err)
	})

	mux.HandleFunc(fmt.Sprintf("/%s.AllocateNetwork", driverapi.NetworkPluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		err := json.NewDecoder(r.Body).Decode(&remoteDriverNetworkRequest)
		if err != nil {
			http.Error(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", plugins.VersionMimetype)
		_, err = fmt.Fprint(w, "null")
		assert.NilError(t, err)
	})

	mux.HandleFunc(fmt.Sprintf("/%s.FreeNetwork", driverapi.NetworkPluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", plugins.VersionMimetype)
		_, err := fmt.Fprint(w, "null")
		assert.NilError(t, err)
	})

	mux.HandleFunc(fmt.Sprintf("/%s.CreateNetwork", driverapi.NetworkPluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		err := json.NewDecoder(r.Body).Decode(&remoteDriverNetworkRequest)
		if err != nil {
			http.Error(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", plugins.VersionMimetype)
		_, err = fmt.Fprint(w, "null")
		assert.NilError(t, err)
	})

	mux.HandleFunc(fmt.Sprintf("/%s.DeleteNetwork", driverapi.NetworkPluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", plugins.VersionMimetype)
		_, err := fmt.Fprint(w, "null")
		assert.NilError(t, err)
	})

	mux.HandleFunc(fmt.Sprintf("/%s.CreateEndpoint", driverapi.NetworkPluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", plugins.VersionMimetype)
		_, err := fmt.Fprint(w, `{"Interface":{"MacAddress":"a0:b1:c2:d3:e4:f5"}}`)
		assert.NilError(t, err)
	})

	mux.HandleFunc(fmt.Sprintf("/%s.Join", driverapi.NetworkPluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", plugins.VersionMimetype)

		veth := &netlink.Veth{
			LinkAttrs: netlink.LinkAttrs{Name: "randomIfName", TxQLen: 0}, PeerName: "cnt0",
		}
		if err := netlink.LinkAdd(veth); err != nil {
			_, err = fmt.Fprint(w, `{"Error":"failed to add veth pair: `+err.Error()+`"}`)
			assert.NilError(t, err)
		} else {
			_, err = fmt.Fprint(w, `{"InterfaceName":{ "SrcName":"cnt0", "DstPrefix":"veth"}}`)
			assert.NilError(t, err)
		}
	})

	mux.HandleFunc(fmt.Sprintf("/%s.Leave", driverapi.NetworkPluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", plugins.VersionMimetype)
		_, err := fmt.Fprint(w, "null")
		assert.NilError(t, err)
	})

	mux.HandleFunc(fmt.Sprintf("/%s.DeleteEndpoint", driverapi.NetworkPluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", plugins.VersionMimetype)
		if link, err := nlwrap.LinkByName("cnt0"); err == nil {
			err := netlink.LinkDel(link)
			assert.NilError(t, err)
		}
		_, err := fmt.Fprint(w, "null")
		assert.NilError(t, err)
	})

	// IPAM Driver implementation
	var (
		poolRequest       remoteipam.RequestPoolRequest
		poolReleaseReq    remoteipam.ReleasePoolRequest
		addressRequest    remoteipam.RequestAddressRequest
		addressReleaseReq remoteipam.ReleaseAddressRequest
	)
	const (
		lAS    = "localAS"
		gAS    = "globalAS"
		pool   = "172.28.0.0/16"
		poolID = lAS + "/" + pool
		gw     = "172.28.255.254/16"
	)

	mux.HandleFunc(fmt.Sprintf("/%s.GetDefaultAddressSpaces", ipamapi.PluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", plugins.VersionMimetype)
		_, err := fmt.Fprint(w, `{"LocalDefaultAddressSpace":"`+lAS+`", "GlobalDefaultAddressSpace": "`+gAS+`"}`)
		assert.NilError(t, err)
	})

	mux.HandleFunc(fmt.Sprintf("/%s.RequestPool", ipamapi.PluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		err := json.NewDecoder(r.Body).Decode(&poolRequest)
		if err != nil {
			http.Error(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", plugins.VersionMimetype)
		if poolRequest.AddressSpace != lAS && poolRequest.AddressSpace != gAS {
			_, err := fmt.Fprint(w, `{"Error":"Unknown address space in pool request: `+poolRequest.AddressSpace+`"}`)
			assert.NilError(t, err)
		} else if poolRequest.Pool != "" && poolRequest.Pool != pool {
			_, err := fmt.Fprint(w, `{"Error":"Cannot handle explicit pool requests yet"}`)
			assert.NilError(t, err)
		} else {
			_, err := fmt.Fprint(w, `{"PoolID":"`+poolID+`", "Pool":"`+pool+`"}`)
			assert.NilError(t, err)
		}
	})

	mux.HandleFunc(fmt.Sprintf("/%s.RequestAddress", ipamapi.PluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		err := json.NewDecoder(r.Body).Decode(&addressRequest)
		if err != nil {
			http.Error(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", plugins.VersionMimetype)
		// make sure libnetwork is now querying on the expected pool id
		if addressRequest.PoolID != poolID {
			_, err := fmt.Fprint(w, `{"Error":"unknown pool id"}`)
			assert.NilError(t, err)
		} else if addressRequest.Address != "" {
			_, err := fmt.Fprint(w, `{"Error":"Cannot handle explicit address requests yet"}`)
			assert.NilError(t, err)
		} else {
			_, err := fmt.Fprint(w, `{"Address":"`+gw+`"}`)
			assert.NilError(t, err)
		}
	})

	mux.HandleFunc(fmt.Sprintf("/%s.ReleaseAddress", ipamapi.PluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		err := json.NewDecoder(r.Body).Decode(&addressReleaseReq)
		if err != nil {
			http.Error(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", plugins.VersionMimetype)
		// make sure libnetwork is now asking to release the expected address from the expected poolid
		if addressRequest.PoolID != poolID {
			_, err := fmt.Fprint(w, `{"Error":"unknown pool id"}`)
			assert.NilError(t, err)
		} else if addressReleaseReq.Address != gw {
			_, err := fmt.Fprint(w, `{"Error":"unknown address"}`)
			assert.NilError(t, err)
		} else {
			_, err := fmt.Fprint(w, "null")
			assert.NilError(t, err)
		}
	})

	mux.HandleFunc(fmt.Sprintf("/%s.ReleasePool", ipamapi.PluginEndpointType), func(w http.ResponseWriter, r *http.Request) {
		err := json.NewDecoder(r.Body).Decode(&poolReleaseReq)
		if err != nil {
			http.Error(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", plugins.VersionMimetype)
		// make sure libnetwork is now asking to release the expected poolid
		if addressRequest.PoolID != poolID {
			_, err := fmt.Fprint(w, `{"Error":"unknown pool id"}`)
			assert.NilError(t, err)
		} else {
			_, err := fmt.Fprint(w, "null")
			assert.NilError(t, err)
		}
	})

	err := os.MkdirAll("/etc/docker/plugins", 0o755)
	assert.NilError(t, err)

	fileName := fmt.Sprintf("/etc/docker/plugins/%s.spec", netDrv)
	err = os.WriteFile(fileName, []byte(url), 0o644)
	assert.NilError(t, err)

	ipamFileName := fmt.Sprintf("/etc/docker/plugins/%s.spec", ipamDrv)
	err = os.WriteFile(ipamFileName, []byte(url), 0o644)
	assert.NilError(t, err)
}

func (s *DockerSwarmSuite) TestSwarmNetworkPlugin(c *testing.T) {
	ctx := testutil.GetContext(c)
	mux := http.NewServeMux()
	s.server = httptest.NewServer(mux)
	assert.Assert(c, s.server != nil) // check that HTTP server has started
	setupRemoteGlobalNetworkPlugin(c, mux, s.server.URL, globalNetworkPlugin, globalIPAMPlugin)
	defer func() {
		s.server.Close()
		err := os.RemoveAll("/etc/docker/plugins")
		assert.NilError(c, err)
	}()

	d := s.AddDaemon(ctx, c, true, true)

	out, err := d.Cmd("network", "create", "-d", globalNetworkPlugin, "foo")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, strings.Contains(out, "not supported in swarm mode"), out)
}

// Test case for #24712
func (s *DockerSwarmSuite) TestSwarmServiceEnvFile(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	path := filepath.Join(d.Folder, "env.txt")
	err := os.WriteFile(path, []byte("VAR1=A\nVAR2=A\n"), 0o644)
	assert.NilError(c, err)

	name := "worker"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--env-file", path, "--env", "VAR1=B", "--env", "VAR1=C", "--env", "VAR2=", "--env", "VAR2", "--name", name, "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	// The complete env is [VAR1=A VAR2=A VAR1=B VAR1=C VAR2= VAR2] and duplicates will be removed => [VAR1=C VAR2]
	out, err = d.Cmd("inspect", "--format", "{{ .Spec.TaskTemplate.ContainerSpec.Env }}", name)
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(out, "[VAR1=C VAR2]"), out)
}

func (s *DockerSwarmSuite) TestSwarmServiceTTY(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	name := "top"

	ttyCheck := "if [ -t 0 ]; then echo TTY > /status; else echo none > /status; fi; exec top"

	// Without --tty
	expectedOutput := "none"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "busybox", "sh", "-c", ttyCheck)
	assert.NilError(c, err, out)

	// Make sure task has been deployed.
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))

	// We need to get the container id.
	out, err = d.Cmd("ps", "-q", "--no-trunc")
	assert.NilError(c, err, out)
	id := strings.TrimSpace(out)

	out, err = d.Cmd("exec", id, "cat", "/status")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(out, expectedOutput), "Expected '%s', but got %q", expectedOutput, out)
	// Remove service
	out, err = d.Cmd("service", "rm", name)
	assert.NilError(c, err, out)
	// Make sure container has been destroyed.
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(0)), poll.WithTimeout(defaultReconciliationTimeout))

	// With --tty
	expectedOutput = "TTY"
	out, err = d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "--tty", "busybox", "sh", "-c", ttyCheck)
	assert.NilError(c, err, out)

	// Make sure task has been deployed.
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))

	// We need to get the container id.
	out, err = d.Cmd("ps", "-q", "--no-trunc")
	assert.NilError(c, err, out)
	id = strings.TrimSpace(out)

	out, err = d.Cmd("exec", id, "cat", "/status")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(out, expectedOutput), "Expected '%s', but got %q", expectedOutput, out)
}

func (s *DockerSwarmSuite) TestSwarmServiceTTYUpdate(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	// Create a service
	name := "top"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "busybox", "top")
	assert.NilError(c, err, out)

	// Make sure task has been deployed.
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))

	out, err = d.Cmd("service", "inspect", "--format", "{{ .Spec.TaskTemplate.ContainerSpec.TTY }}", name)
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "false")

	out, err = d.Cmd("service", "update", "--detach", "--tty", name)
	assert.NilError(c, err, out)

	out, err = d.Cmd("service", "inspect", "--format", "{{ .Spec.TaskTemplate.ContainerSpec.TTY }}", name)
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "true")
}

func (s *DockerSwarmSuite) TestSwarmServiceNetworkUpdate(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	result := icmd.RunCmd(d.Command("network", "create", "-d", "overlay", "foo"))
	result.Assert(c, icmd.Success)
	fooNetwork := strings.TrimSpace(result.Combined())

	result = icmd.RunCmd(d.Command("network", "create", "-d", "overlay", "bar"))
	result.Assert(c, icmd.Success)
	barNetwork := strings.TrimSpace(result.Combined())

	result = icmd.RunCmd(d.Command("network", "create", "-d", "overlay", "baz"))
	result.Assert(c, icmd.Success)
	bazNetwork := strings.TrimSpace(result.Combined())

	// Create a service
	name := "top"
	result = icmd.RunCmd(d.Command("service", "create", "--detach", "--no-resolve-image", "--network", "foo", "--network", "bar", "--name", name, "busybox", "top"))
	result.Assert(c, icmd.Success)

	// Make sure task has been deployed.
	poll.WaitOn(c, pollCheck(c, d.CheckRunningTaskNetworks(ctx), checker.DeepEquals(map[string]int{fooNetwork: 1, barNetwork: 1})), poll.WithTimeout(defaultReconciliationTimeout))

	// Remove a network
	result = icmd.RunCmd(d.Command("service", "update", "--detach", "--network-rm", "foo", name))
	result.Assert(c, icmd.Success)

	poll.WaitOn(c, pollCheck(c, d.CheckRunningTaskNetworks(ctx), checker.DeepEquals(map[string]int{barNetwork: 1})), poll.WithTimeout(defaultReconciliationTimeout))

	// Add a network
	result = icmd.RunCmd(d.Command("service", "update", "--detach", "--network-add", "baz", name))
	result.Assert(c, icmd.Success)

	poll.WaitOn(c, pollCheck(c, d.CheckRunningTaskNetworks(ctx), checker.DeepEquals(map[string]int{barNetwork: 1, bazNetwork: 1})), poll.WithTimeout(defaultReconciliationTimeout))
}

func (s *DockerSwarmSuite) TestDNSConfig(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	// Create a service
	name := "top"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "--dns=1.2.3.4", "--dns-search=example.com", "--dns-option=timeout:3", "busybox", "top")
	assert.NilError(c, err, out)

	// Make sure task has been deployed.
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))

	// We need to get the container id.
	out, err = d.Cmd("ps", "-a", "-q", "--no-trunc")
	assert.NilError(c, err, out)
	id := strings.TrimSpace(out)

	// Compare against expected output.
	expectedOutput1 := "nameserver 1.2.3.4"
	expectedOutput2 := "search example.com"
	expectedOutput3 := "options timeout:3"
	out, err = d.Cmd("exec", id, "cat", "/etc/resolv.conf")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(out, expectedOutput1), "Expected '%s', but got %q", expectedOutput1, out)
	assert.Assert(c, strings.Contains(out, expectedOutput2), "Expected '%s', but got %q", expectedOutput2, out)
	assert.Assert(c, strings.Contains(out, expectedOutput3), "Expected '%s', but got %q", expectedOutput3, out)
}

func (s *DockerSwarmSuite) TestDNSConfigUpdate(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	// Create a service
	name := "top"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "busybox", "top")
	assert.NilError(c, err, out)

	// Make sure task has been deployed.
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))

	out, err = d.Cmd("service", "update", "--detach", "--dns-add=1.2.3.4", "--dns-search-add=example.com", "--dns-option-add=timeout:3", name)
	assert.NilError(c, err, out)

	out, err = d.Cmd("service", "inspect", "--format", "{{ .Spec.TaskTemplate.ContainerSpec.DNSConfig }}", name)
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "{[1.2.3.4] [example.com] [timeout:3]}")
}

func getNodeStatus(t *testing.T, d *daemon.Daemon) swarm.LocalNodeState {
	ctx := testutil.GetContext(t)
	info := d.SwarmInfo(ctx, t)
	return info.LocalNodeState
}

func checkKeyIsEncrypted(d *daemon.Daemon) func(*testing.T) (interface{}, string) {
	return func(t *testing.T) (interface{}, string) {
		keyBytes, err := os.ReadFile(filepath.Join(d.Folder, "root", "swarm", "certificates", "swarm-node.key"))
		if err != nil {
			return fmt.Errorf("error reading key: %v", err), ""
		}

		keyBlock, _ := pem.Decode(keyBytes)
		if keyBlock == nil {
			return errors.New("invalid PEM-encoded private key"), ""
		}

		return keyutils.IsEncryptedPEMBlock(keyBlock), ""
	}
}

func checkSwarmLockedToUnlocked(ctx context.Context, t *testing.T, d *daemon.Daemon) {
	// Wait for the PEM file to become unencrypted
	poll.WaitOn(t, pollCheck(t, checkKeyIsEncrypted(d), checker.Equals(false)), poll.WithTimeout(defaultReconciliationTimeout))

	d.RestartNode(t)
	poll.WaitOn(t, pollCheck(t, d.CheckLocalNodeState(ctx), checker.Equals(swarm.LocalNodeStateActive)), poll.WithTimeout(time.Second))
}

func checkSwarmUnlockedToLocked(ctx context.Context, t *testing.T, d *daemon.Daemon) {
	// Wait for the PEM file to become encrypted
	poll.WaitOn(t, pollCheck(t, checkKeyIsEncrypted(d), checker.Equals(true)), poll.WithTimeout(defaultReconciliationTimeout))

	d.RestartNode(t)
	poll.WaitOn(t, pollCheck(t, d.CheckLocalNodeState(ctx), checker.Equals(swarm.LocalNodeStateLocked)), poll.WithTimeout(time.Second))
}

func (s *DockerSwarmSuite) TestUnlockEngineAndUnlockedSwarm(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, false, false)

	// unlocking a normal engine should return an error - it does not even ask for the key
	cmd := d.Command("swarm", "unlock")
	result := icmd.RunCmd(cmd)
	result.Assert(c, icmd.Expected{
		ExitCode: 1,
	})
	out := result.Combined()
	assert.Assert(c, strings.Contains(result.Combined(), "Error: This node is not part of a swarm"), out)
	assert.Assert(c, !strings.Contains(result.Combined(), "Please enter unlock key"), out)
	out, err := d.Cmd("swarm", "init")
	assert.NilError(c, err, out)

	// unlocking an unlocked swarm should return an error - it does not even ask for the key
	cmd = d.Command("swarm", "unlock")
	result = icmd.RunCmd(cmd)
	result.Assert(c, icmd.Expected{
		ExitCode: 1,
	})
	out = result.Combined()
	assert.Assert(c, strings.Contains(result.Combined(), "Error: swarm is not locked"), out)
	assert.Assert(c, !strings.Contains(result.Combined(), "Please enter unlock key"), out)
}

func (s *DockerSwarmSuite) TestSwarmInitLocked(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, false, false)

	outs, err := d.Cmd("swarm", "init", "--autolock")
	assert.Assert(c, err == nil, outs)
	unlockKey := getUnlockKey(d, c, outs)

	assert.Equal(c, getNodeStatus(c, d), swarm.LocalNodeStateActive)

	// It starts off locked
	d.RestartNode(c)
	assert.Equal(c, getNodeStatus(c, d), swarm.LocalNodeStateLocked)

	cmd := d.Command("swarm", "unlock")
	cmd.Stdin = bytes.NewBufferString("wrong-secret-key")
	icmd.RunCmd(cmd).Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "invalid key",
	})

	assert.Equal(c, getNodeStatus(c, d), swarm.LocalNodeStateLocked)

	cmd = d.Command("swarm", "unlock")
	cmd.Stdin = bytes.NewBufferString(unlockKey)
	icmd.RunCmd(cmd).Assert(c, icmd.Success)

	assert.Equal(c, getNodeStatus(c, d), swarm.LocalNodeStateActive)

	outs, err = d.Cmd("node", "ls")
	assert.Assert(c, err == nil, outs)
	assert.Assert(c, !strings.Contains(outs, "Swarm is encrypted and needs to be unlocked"), outs)
	outs, err = d.Cmd("swarm", "update", "--autolock=false")
	assert.Assert(c, err == nil, outs)

	checkSwarmLockedToUnlocked(ctx, c, d)

	outs, err = d.Cmd("node", "ls")
	assert.Assert(c, err == nil, outs)
	assert.Assert(c, !strings.Contains(outs, "Swarm is encrypted and needs to be unlocked"), outs)
}

func (s *DockerSwarmSuite) TestSwarmLeaveLocked(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, false, false)

	outs, err := d.Cmd("swarm", "init", "--autolock")
	assert.Assert(c, err == nil, outs)

	// It starts off locked
	d.RestartNode(c)

	info := d.SwarmInfo(ctx, c)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateLocked)

	outs, _ = d.Cmd("node", "ls")
	assert.Assert(c, strings.Contains(outs, "Swarm is encrypted and needs to be unlocked"), outs)
	// `docker swarm leave` a locked swarm without --force will return an error
	outs, _ = d.Cmd("swarm", "leave")
	assert.Assert(c, strings.Contains(outs, "Swarm is encrypted and locked."), outs)
	// It is OK for user to leave a locked swarm with --force
	outs, err = d.Cmd("swarm", "leave", "--force")
	assert.Assert(c, err == nil, outs)

	info = d.SwarmInfo(ctx, c)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateInactive)

	outs, err = d.Cmd("swarm", "init")
	assert.Assert(c, err == nil, outs)

	info = d.SwarmInfo(ctx, c)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateActive)
}

func (s *DockerSwarmSuite) TestSwarmLockUnlockCluster(c *testing.T) {
	ctx := testutil.GetContext(c)
	d1 := s.AddDaemon(ctx, c, true, true)
	d2 := s.AddDaemon(ctx, c, true, true)
	d3 := s.AddDaemon(ctx, c, true, true)

	// they start off unlocked
	d2.RestartNode(c)
	assert.Equal(c, getNodeStatus(c, d2), swarm.LocalNodeStateActive)

	// stop this one so it does not get autolock info
	d2.Stop(c)

	// enable autolock
	outs, err := d1.Cmd("swarm", "update", "--autolock")
	assert.Assert(c, err == nil, outs)
	unlockKey := getUnlockKey(d1, c, outs)

	// The ones that got the cluster update should be set to locked
	for _, d := range []*daemon.Daemon{d1, d3} {
		checkSwarmUnlockedToLocked(ctx, c, d)

		cmd := d.Command("swarm", "unlock")
		cmd.Stdin = bytes.NewBufferString(unlockKey)
		icmd.RunCmd(cmd).Assert(c, icmd.Success)
		assert.Equal(c, getNodeStatus(c, d), swarm.LocalNodeStateActive)
	}

	// d2 never got the cluster update, so it is still set to unlocked
	d2.StartNode(c)
	assert.Equal(c, getNodeStatus(c, d2), swarm.LocalNodeStateActive)

	// d2 is now set to lock
	checkSwarmUnlockedToLocked(ctx, c, d2)

	// leave it locked, and set the cluster to no longer autolock
	outs, err = d1.Cmd("swarm", "update", "--autolock=false")
	assert.Assert(c, err == nil, "out: %v", outs)

	// the ones that got the update are now set to unlocked
	for _, d := range []*daemon.Daemon{d1, d3} {
		checkSwarmLockedToUnlocked(ctx, c, d)
	}

	// d2 still locked
	assert.Equal(c, getNodeStatus(c, d2), swarm.LocalNodeStateLocked)

	// unlock it
	cmd := d2.Command("swarm", "unlock")
	cmd.Stdin = bytes.NewBufferString(unlockKey)
	icmd.RunCmd(cmd).Assert(c, icmd.Success)
	assert.Equal(c, getNodeStatus(c, d2), swarm.LocalNodeStateActive)

	// once it's caught up, d2 is set to not be locked
	checkSwarmLockedToUnlocked(ctx, c, d2)

	// managers who join now are never set to locked in the first place
	d4 := s.AddDaemon(ctx, c, true, true)
	d4.RestartNode(c)
	assert.Equal(c, getNodeStatus(c, d4), swarm.LocalNodeStateActive)
}

func (s *DockerSwarmSuite) TestSwarmJoinPromoteLocked(c *testing.T) {
	ctx := testutil.GetContext(c)
	d1 := s.AddDaemon(ctx, c, true, true)

	// enable autolock
	outs, err := d1.Cmd("swarm", "update", "--autolock")
	assert.Assert(c, err == nil, "out: %v", outs)
	unlockKey := getUnlockKey(d1, c, outs)

	// joined workers start off unlocked
	d2 := s.AddDaemon(ctx, c, true, false)
	d2.RestartNode(c)
	poll.WaitOn(c, pollCheck(c, d2.CheckLocalNodeState(ctx), checker.Equals(swarm.LocalNodeStateActive)), poll.WithTimeout(time.Second))

	// promote worker
	outs, err = d1.Cmd("node", "promote", d2.NodeID())
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(outs, "promoted to a manager in the swarm"), outs)
	// join new manager node
	d3 := s.AddDaemon(ctx, c, true, true)

	// both new nodes are locked
	for _, d := range []*daemon.Daemon{d2, d3} {
		checkSwarmUnlockedToLocked(ctx, c, d)

		cmd := d.Command("swarm", "unlock")
		cmd.Stdin = bytes.NewBufferString(unlockKey)
		icmd.RunCmd(cmd).Assert(c, icmd.Success)
		assert.Equal(c, getNodeStatus(c, d), swarm.LocalNodeStateActive)
	}

	// demote manager back to worker - workers are not locked
	outs, err = d1.Cmd("node", "demote", d3.NodeID())
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(outs, "demoted in the swarm"), outs)
	// Wait for it to actually be demoted, for the key and cert to be replaced.
	// Then restart and assert that the node is not locked.  If we don't wait for the cert
	// to be replaced, then the node still has the manager TLS key which is still locked
	// (because we never want a manager TLS key to be on disk unencrypted if the cluster
	// is set to autolock)
	poll.WaitOn(c, pollCheck(c, d3.CheckControlAvailable(ctx), checker.False()), poll.WithTimeout(defaultReconciliationTimeout))
	poll.WaitOn(c, pollCheck(c, func(t *testing.T) (interface{}, string) {
		certBytes, err := os.ReadFile(filepath.Join(d3.Folder, "root", "swarm", "certificates", "swarm-node.crt"))
		if err != nil {
			return "", fmt.Sprintf("error: %v", err)
		}
		certs, err := helpers.ParseCertificatesPEM(certBytes)
		if err == nil && len(certs) > 0 && len(certs[0].Subject.OrganizationalUnit) > 0 {
			return certs[0].Subject.OrganizationalUnit[0], ""
		}
		return "", "could not get organizational unit from certificate"
	}, checker.Equals("swarm-worker")), poll.WithTimeout(defaultReconciliationTimeout))

	// by now, it should *never* be locked on restart
	d3.RestartNode(c)
	poll.WaitOn(c, pollCheck(c, d3.CheckLocalNodeState(ctx), checker.Equals(swarm.LocalNodeStateActive)), poll.WithTimeout(time.Second))
}

const swarmIsEncryptedMsg = "Swarm is encrypted and needs to be unlocked"

func (s *DockerSwarmSuite) TestSwarmRotateUnlockKey(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	outs, err := d.Cmd("swarm", "update", "--autolock")
	assert.Assert(c, err == nil, "out: %v", outs)
	unlockKey := getUnlockKey(d, c, outs)

	// Rotate multiple times
	for i := 0; i != 3; i++ {
		outs, err = d.Cmd("swarm", "unlock-key", "-q", "--rotate")
		assert.Assert(c, err == nil, "out: %v", outs)
		// Strip \n
		newUnlockKey := outs[:len(outs)-1]
		assert.Assert(c, newUnlockKey != "")
		assert.Assert(c, newUnlockKey != unlockKey)

		d.RestartNode(c)
		assert.Equal(c, getNodeStatus(c, d), swarm.LocalNodeStateLocked)

		unlock := func(d *daemon.Daemon, key string) *icmd.Result {
			cmd := d.Command("swarm", "unlock")
			cmd.Stdin = strings.NewReader(key)
			return icmd.RunCmd(cmd)
		}

		outs, _ = d.Cmd("node", "ls")
		assert.Assert(c, strings.Contains(outs, swarmIsEncryptedMsg), outs)

		result := unlock(d, unlockKey)
		if result.Error == nil {
			// On occasion, the daemon may not have finished
			// rotating the KEK before restarting. The test is
			// intentionally written to explore this behavior.
			// When this happens, unlocking with the old key will
			// succeed. If we wait for the rotation to happen and
			// restart again, the new key should be required this
			// time.

			// Wait for the rotation to happen
			// Since there are multiple rotations, we need to wait until for the number of rotations we are currently on to be reflected in the logs
			// This is a little janky... its depending on specific log messages AND these are debug logs... but it is the best we can do for now.
			matcher := testdaemon.ScanLogsMatchCount(testdaemon.ScanLogsMatchString("successfully rotated KEK"), i+1)
			poll.WaitOn(c, d.PollCheckLogs(ctx, matcher), poll.WithDelay(3*time.Second), poll.WithTimeout(time.Minute))
			d.Restart(c)

			d.RestartNode(c)

			result = unlock(d, unlockKey)
		}
		result.Assert(c, icmd.Expected{
			ExitCode: 1,
			Err:      "invalid key",
		})

		outs, _ = d.Cmd("node", "ls")
		assert.Assert(c, strings.Contains(outs, swarmIsEncryptedMsg), outs)
		unlock(d, newUnlockKey).Assert(c, icmd.Success)

		assert.Equal(c, getNodeStatus(c, d), swarm.LocalNodeStateActive)

		checkNodeLs := func(t poll.LogT) poll.Result {
			// an issue sometimes prevents leader to be available right away
			out, err := d.Cmd("node", "ls")
			if err != nil {
				return poll.Continue("error running node ls: %v: %s", err, out)
			}
			return poll.Success()
		}
		poll.WaitOn(c, checkNodeLs, poll.WithDelay(3*time.Second), poll.WithTimeout(time.Minute))

		unlockKey = newUnlockKey
	}
}

// This differs from `TestSwarmRotateUnlockKey` because that one rotates a single node, which is the leader.
// This one keeps the leader up, and asserts that other manager nodes in the cluster also have their unlock
// key rotated.
func (s *DockerSwarmSuite) TestSwarmClusterRotateUnlockKey(c *testing.T) {
	if runtime.GOARCH == "s390x" {
		c.Skip("Disabled on s390x")
	}
	if runtime.GOARCH == "ppc64le" {
		c.Skip("Disabled on  ppc64le")
	}
	ctx := testutil.GetContext(c)

	d1 := s.AddDaemon(ctx, c, true, true) // leader - don't restart this one, we don't want leader election delays
	d2 := s.AddDaemon(ctx, c, true, true)
	d3 := s.AddDaemon(ctx, c, true, true)

	outs, err := d1.Cmd("swarm", "update", "--autolock")
	assert.Assert(c, err == nil, outs)
	unlockKey := getUnlockKey(d1, c, outs)

	// Rotate multiple times
	for i := 0; i != 3; i++ {
		outs, err = d1.Cmd("swarm", "unlock-key", "-q", "--rotate")
		assert.Assert(c, err == nil, outs)
		// Strip \n
		newUnlockKey := outs[:len(outs)-1]
		assert.Assert(c, newUnlockKey != "")
		assert.Assert(c, newUnlockKey != unlockKey)

		d2.RestartNode(c)
		d3.RestartNode(c)

		unlock := func(d *daemon.Daemon, key string) *icmd.Result {
			cmd := d.Command("swarm", "unlock")
			cmd.Stdin = strings.NewReader(key)
			return icmd.RunCmd(cmd)
		}

		const swarmIsEncryptedMsg = "Swarm is encrypted and needs to be unlocked"

		for _, d := range []*daemon.Daemon{d2, d3} {
			assert.Equal(c, getNodeStatus(c, d), swarm.LocalNodeStateLocked)

			outs, _ := d.Cmd("node", "ls")
			assert.Assert(c, strings.Contains(outs, swarmIsEncryptedMsg), outs)

			// unlock with the original key should fail
			// Use poll here because the daemon may not have finished
			result := unlock(d, unlockKey)
			if result.Error == nil {
				// On occasion, the daemon may not have finished
				// rotating the KEK before restarting. The test is
				// intentionally written to explore this behavior.
				// When this happens, unlocking with the old key will
				// succeed. If we wait for the rotation to happen and
				// restart again, the new key should be required this
				// time.

				// Wait for the rotation to happen
				// Since there are multiple rotations, we need to wait until for the number of rotations we are currently on to be reflected in the logs
				// This is a little janky... its depending on specific log messages AND these are debug logs... but it is the best we can do for now.
				matcher := testdaemon.ScanLogsMatchCount(testdaemon.ScanLogsMatchString("successfully rotated KEK"), i+1)
				poll.WaitOn(c, d.PollCheckLogs(ctx, matcher), poll.WithDelay(3*time.Second), poll.WithTimeout(time.Minute))
				d.Restart(c)

				result = unlock(d, unlockKey)
			}
			result.Assert(c, icmd.Expected{
				ExitCode: 1,
				Err:      "invalid key",
			})

			outs, _ = d.Cmd("node", "ls")
			assert.Assert(c, strings.Contains(outs, swarmIsEncryptedMsg), outs)

			// now unlock with the rotated key, this should succeed
			unlock(d, newUnlockKey).Assert(c, icmd.Success)
			assert.Equal(c, getNodeStatus(c, d), swarm.LocalNodeStateActive)

			checkNodeLs := func(t poll.LogT) poll.Result {
				// an issue sometimes prevents leader to be available right away
				out, err := d.Cmd("node", "ls")
				if err != nil {
					return poll.Continue("error running node ls: %v: %s", err, out)
				}
				return poll.Success()
			}
			poll.WaitOn(c, checkNodeLs, poll.WithDelay(3*time.Second), poll.WithTimeout(time.Minute))
		}

		unlockKey = newUnlockKey
	}
}

func (s *DockerSwarmSuite) TestSwarmAlternateLockUnlock(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	for i := 0; i < 2; i++ {
		// set to lock
		outs, err := d.Cmd("swarm", "update", "--autolock")
		assert.Assert(c, err == nil, "out: %v", outs)
		assert.Assert(c, strings.Contains(outs, "docker swarm unlock"), outs)
		unlockKey := getUnlockKey(d, c, outs)

		checkSwarmUnlockedToLocked(ctx, c, d)

		cmd := d.Command("swarm", "unlock")
		cmd.Stdin = bytes.NewBufferString(unlockKey)
		icmd.RunCmd(cmd).Assert(c, icmd.Success)

		assert.Equal(c, getNodeStatus(c, d), swarm.LocalNodeStateActive)

		outs, err = d.Cmd("swarm", "update", "--autolock=false")
		assert.Assert(c, err == nil, "out: %v", outs)

		checkSwarmLockedToUnlocked(ctx, c, d)
	}
}

func (s *DockerSwarmSuite) TestExtraHosts(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	// Create a service
	name := "top"
	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", name, "--host=example.com:1.2.3.4", "busybox", "top")
	assert.NilError(c, err, out)

	// Make sure task has been deployed.
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))

	// We need to get the container id.
	out, err = d.Cmd("ps", "-a", "-q", "--no-trunc")
	assert.NilError(c, err, out)
	id := strings.TrimSpace(out)

	// Compare against expected output.
	expectedOutput := "1.2.3.4\texample.com"
	out, err = d.Cmd("exec", id, "cat", "/etc/hosts")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(out, expectedOutput), "Expected '%s', but got %q", expectedOutput, out)
}

func (s *DockerSwarmSuite) TestSwarmManagerAddress(c *testing.T) {
	ctx := testutil.GetContext(c)
	d1 := s.AddDaemon(ctx, c, true, true)
	d2 := s.AddDaemon(ctx, c, true, false)
	d3 := s.AddDaemon(ctx, c, true, false)

	// Manager Addresses will always show Node 1's address
	expectedOutput := fmt.Sprintf("127.0.0.1:%d", d1.SwarmPort)

	out, err := d1.Cmd("info", "--format", "{{ (index .Swarm.RemoteManagers 0).Addr }}")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(out, expectedOutput), out)

	out, err = d2.Cmd("info", "--format", "{{ (index .Swarm.RemoteManagers 0).Addr }}")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(out, expectedOutput), out)

	out, err = d3.Cmd("info", "--format", "{{ (index .Swarm.RemoteManagers 0).Addr }}")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(out, expectedOutput), out)
}

func (s *DockerSwarmSuite) TestSwarmNetworkIPAMOptions(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	out, err := d.Cmd("network", "create", "-d", "overlay", "--ipam-opt", "foo=bar", "foo")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	out, err = d.Cmd("network", "inspect", "--format", "{{.IPAM.Options}}", "foo")
	out = strings.TrimSpace(out)
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(out, "foo:bar"), out)
	assert.Assert(c, strings.Contains(out, "com.docker.network.ipam.serial:true"), out)
	out, err = d.Cmd("service", "create", "--detach", "--no-resolve-image", "--network=foo", "--name", "top", "busybox", "top")
	assert.NilError(c, err, out)

	// make sure task has been deployed.
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))

	out, err = d.Cmd("network", "inspect", "--format", "{{.IPAM.Options}}", "foo")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(out, "foo:bar"), out)
	assert.Assert(c, strings.Contains(out, "com.docker.network.ipam.serial:true"), out)
}

// Test case for issue #27866, which did not allow NW name that is the prefix of a swarm NW ID.
// e.g. if the ingress ID starts with "n1", it was impossible to create a NW named "n1".
func (s *DockerSwarmSuite) TestSwarmNetworkCreateIssue27866(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)
	out, err := d.Cmd("network", "inspect", "-f", "{{.Id}}", "ingress")
	assert.NilError(c, err, "out: %v", out)
	ingressID := strings.TrimSpace(out)
	assert.Assert(c, ingressID != "")

	// create a network of which name is the prefix of the ID of an overlay network
	// (ingressID in this case)
	newNetName := ingressID[0:2]
	out, err = d.Cmd("network", "create", "--driver", "overlay", newNetName)
	// In #27866, it was failing because of "network with name %s already exists"
	assert.NilError(c, err, "out: %v", out)
	out, err = d.Cmd("network", "rm", newNetName)
	assert.NilError(c, err, "out: %v", out)
}

// Test case for https://github.com/moby/moby/pull/27938#issuecomment-265768303
// This test creates two networks with the same name sequentially, with various drivers.
// Since the operations in this test are done sequentially, the 2nd call should fail with
// "network with name FOO already exists".
// Note that it is to ok have multiple networks with the same name if the operations are done
// in parallel. (#18864)
func (s *DockerSwarmSuite) TestSwarmNetworkCreateDup(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)
	drivers := []string{"bridge", "overlay"}
	for i, driver1 := range drivers {
		for _, driver2 := range drivers {
			c.Run(fmt.Sprintf("driver %s then %s", driver1, driver2), func(c *testing.T) {
				nwName := fmt.Sprintf("network-test-%d", i)
				out, err := d.Cmd("network", "create", "--driver", driver1, nwName)
				assert.NilError(c, err, "out: %v", out)
				out, err = d.Cmd("network", "create", "--driver", driver2, nwName)
				assert.Assert(c, strings.Contains(out, fmt.Sprintf("network with name %s already exists", nwName)), out)
				assert.ErrorContains(c, err, "")
				out, err = d.Cmd("network", "rm", nwName)
				assert.NilError(c, err, "out: %v", out)
			})
		}
	}
}

func (s *DockerSwarmSuite) TestSwarmPublishDuplicatePorts(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	out, err := d.Cmd("service", "create", "--no-resolve-image", "--detach=true", "--publish", "5005:80", "--publish", "5006:80", "--publish", "80", "--publish", "80", "busybox", "top")
	assert.NilError(c, err, out)
	id := strings.TrimSpace(out)

	// make sure task has been deployed.
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))

	// Total len = 4, with 2 dynamic ports and 2 non-dynamic ports
	// Dynamic ports are likely to be 30000 and 30001 but doesn't matter
	out, err = d.Cmd("service", "inspect", "--format", "{{.Endpoint.Ports}} len={{len .Endpoint.Ports}}", id)
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(out, "len=4"), out)
	assert.Assert(c, strings.Contains(out, "{ tcp 80 5005 ingress}"), out)
	assert.Assert(c, strings.Contains(out, "{ tcp 80 5006 ingress}"), out)
}

func (s *DockerSwarmSuite) TestSwarmJoinWithDrain(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	out, err := d.Cmd("node", "ls")
	assert.NilError(c, err)
	assert.Assert(c, !strings.Contains(out, "Drain"), out)
	out, err = d.Cmd("swarm", "join-token", "-q", "manager")
	assert.NilError(c, err)
	assert.Assert(c, strings.TrimSpace(out) != "")

	token := strings.TrimSpace(out)

	d1 := s.AddDaemon(ctx, c, false, false)

	out, err = d1.Cmd("swarm", "join", "--availability=drain", "--token", token, d.SwarmListenAddr())
	assert.NilError(c, err)
	assert.Assert(c, strings.TrimSpace(out) != "")

	out, err = d.Cmd("node", "ls")
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, "Drain"), out)
	out, err = d1.Cmd("node", "ls")
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, "Drain"), out)
}

func (s *DockerSwarmSuite) TestSwarmInitWithDrain(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, false, false)

	out, err := d.Cmd("swarm", "init", "--availability", "drain")
	assert.NilError(c, err, "out: %v", out)

	out, err = d.Cmd("node", "ls")
	assert.NilError(c, err)
	assert.Assert(c, is.Contains(out, "Drain"))
}

func (s *DockerSwarmSuite) TestSwarmReadonlyRootfs(c *testing.T) {
	testRequires(c, DaemonIsLinux, UserNamespaceROMount)
	ctx := testutil.GetContext(c)

	d := s.AddDaemon(ctx, c, true, true)

	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", "top", "--read-only", "busybox", "top")
	assert.NilError(c, err, out)

	// make sure task has been deployed.
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))

	out, err = d.Cmd("service", "inspect", "--format", "{{ .Spec.TaskTemplate.ContainerSpec.ReadOnly }}", "top")
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "true")

	containers := d.ActiveContainers(testutil.GetContext(c), c)
	out, err = d.Cmd("inspect", "--type", "container", "--format", "{{.HostConfig.ReadonlyRootfs}}", containers[0])
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "true")
}

func (s *DockerSwarmSuite) TestSwarmStopSignal(c *testing.T) {
	ctx := testutil.GetContext(c)
	testRequires(c, DaemonIsLinux, UserNamespaceROMount)

	d := s.AddDaemon(ctx, c, true, true)

	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", "top", "--stop-signal=SIGHUP", "busybox", "top")
	assert.NilError(c, err, out)

	// make sure task has been deployed.
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))

	out, err = d.Cmd("service", "inspect", "--format", "{{ .Spec.TaskTemplate.ContainerSpec.StopSignal }}", "top")
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "SIGHUP")

	containers := d.ActiveContainers(testutil.GetContext(c), c)
	out, err = d.Cmd("inspect", "--type", "container", "--format", "{{.Config.StopSignal}}", containers[0])
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "SIGHUP")

	out, err = d.Cmd("service", "update", "--detach", "--stop-signal=SIGUSR1", "top")
	assert.NilError(c, err, out)

	out, err = d.Cmd("service", "inspect", "--format", "{{ .Spec.TaskTemplate.ContainerSpec.StopSignal }}", "top")
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), "SIGUSR1")
}

func (s *DockerSwarmSuite) TestSwarmServiceLsFilterMode(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", "top1", "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	out, err = d.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", "top2", "--mode=global", "busybox", "top")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")

	// make sure task has been deployed.
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(2)), poll.WithTimeout(defaultReconciliationTimeout))

	out, err = d.Cmd("service", "ls")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(out, "top1"), out)
	assert.Assert(c, strings.Contains(out, "top2"), out)
	assert.Assert(c, !strings.Contains(out, "localnet"), out)
	out, err = d.Cmd("service", "ls", "--filter", "mode=global")
	assert.Assert(c, !strings.Contains(out, "top1"), out)
	assert.Assert(c, strings.Contains(out, "top2"), out)
	assert.NilError(c, err, out)

	out, err = d.Cmd("service", "ls", "--filter", "mode=replicated")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(out, "top1"), out)
	assert.Assert(c, !strings.Contains(out, "top2"), out)
}

func (s *DockerSwarmSuite) TestSwarmInitUnspecifiedDataPathAddr(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, false, false)

	out, err := d.Cmd("swarm", "init", "--data-path-addr", "0.0.0.0")
	assert.ErrorContains(c, err, "")
	assert.Assert(c, strings.Contains(out, "data path address must be a non-zero IP"), out)
	out, err = d.Cmd("swarm", "init", "--data-path-addr", "0.0.0.0:2000")
	assert.ErrorContains(c, err, "")
	assert.Assert(c, strings.Contains(out, "data path address must be a non-zero IP"), out)
}

func (s *DockerSwarmSuite) TestSwarmJoinLeave(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	out, err := d.Cmd("swarm", "join-token", "-q", "worker")
	assert.NilError(c, err)
	assert.Assert(c, strings.TrimSpace(out) != "")

	token := strings.TrimSpace(out)

	// Verify that back to back join/leave does not cause panics
	d1 := s.AddDaemon(ctx, c, false, false)
	for i := 0; i < 10; i++ {
		out, err = d1.Cmd("swarm", "join", "--token", token, d.SwarmListenAddr())
		assert.NilError(c, err)
		assert.Assert(c, strings.TrimSpace(out) != "")

		_, err = d1.Cmd("swarm", "leave")
		assert.NilError(c, err)
	}
}

const defaultRetryCount = 10

func waitForEvent(t *testing.T, d *daemon.Daemon, since string, filter string, event string, retry int) string {
	if retry < 1 {
		t.Fatalf("retry count %d is invalid. It should be no less than 1", retry)
		return ""
	}
	var out string
	for i := 0; i < retry; i++ {
		until := daemonUnixTime(t)
		var err error
		if filter != "" {
			out, err = d.Cmd("events", "--since", since, "--until", until, filter)
		} else {
			out, err = d.Cmd("events", "--since", since, "--until", until)
		}
		assert.NilError(t, err, out)
		if strings.Contains(out, event) {
			return strings.TrimSpace(out)
		}
		// no need to sleep after last retry
		if i < retry-1 {
			time.Sleep(200 * time.Millisecond)
		}
	}
	t.Fatalf("docker events output '%s' doesn't contain event '%s'", out, event)
	return ""
}

func (s *DockerSwarmSuite) TestSwarmClusterEventsSource(c *testing.T) {
	ctx := testutil.GetContext(c)
	d1 := s.AddDaemon(ctx, c, true, true)
	d2 := s.AddDaemon(ctx, c, true, true)
	d3 := s.AddDaemon(ctx, c, true, false)

	// create a network
	out, err := d1.Cmd("network", "create", "--attachable", "-d", "overlay", "foo")
	assert.NilError(c, err, out)
	networkID := strings.TrimSpace(out)
	assert.Assert(c, networkID != "")

	// d1, d2 are managers that can get swarm events
	waitForEvent(c, d1, "0", "-f scope=swarm", "network create "+networkID, defaultRetryCount)
	waitForEvent(c, d2, "0", "-f scope=swarm", "network create "+networkID, defaultRetryCount)

	// d3 is a worker, not able to get cluster events
	out = waitForEvent(c, d3, "0", "-f scope=swarm", "", 1)
	assert.Assert(c, !strings.Contains(out, "network create "), out)
}

func (s *DockerSwarmSuite) TestSwarmClusterEventsScope(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	// create a service
	out, err := d.Cmd("service", "create", "--no-resolve-image", "--name", "test", "--detach=false", "busybox", "top")
	assert.NilError(c, err, out)
	serviceID := strings.Split(out, "\n")[0]

	// scope swarm filters cluster events
	out = waitForEvent(c, d, "0", "-f scope=swarm", "service create "+serviceID, defaultRetryCount)
	assert.Assert(c, !strings.Contains(out, "container create "), out)
	// all events are returned if scope is not specified
	waitForEvent(c, d, "0", "", "service create "+serviceID, 1)
	waitForEvent(c, d, "0", "", "container create ", defaultRetryCount)

	// scope local only shows non-cluster events
	out = waitForEvent(c, d, "0", "-f scope=local", "container create ", 1)
	assert.Assert(c, !strings.Contains(out, "service create "), out)
}

func (s *DockerSwarmSuite) TestSwarmClusterEventsType(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	// create a service
	out, err := d.Cmd("service", "create", "--no-resolve-image", "--name", "test", "--detach=false", "busybox", "top")
	assert.NilError(c, err, out)
	serviceID := strings.Split(out, "\n")[0]

	// create a network
	out, err = d.Cmd("network", "create", "--attachable", "-d", "overlay", "foo")
	assert.NilError(c, err, out)
	networkID := strings.TrimSpace(out)
	assert.Assert(c, networkID != "")

	// filter by service
	out = waitForEvent(c, d, "0", "-f type=service", "service create "+serviceID, defaultRetryCount)
	assert.Assert(c, !strings.Contains(out, "network create"), out)
	// filter by network
	out = waitForEvent(c, d, "0", "-f type=network", "network create "+networkID, defaultRetryCount)
	assert.Assert(c, !strings.Contains(out, "service create"), out)
}

func (s *DockerSwarmSuite) TestSwarmClusterEventsService(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	// create a service
	out, err := d.Cmd("service", "create", "--no-resolve-image", "--name", "test", "--detach=false", "busybox", "top")
	assert.NilError(c, err, out)
	serviceID := strings.Split(out, "\n")[0]

	// validate service create event
	waitForEvent(c, d, "0", "-f scope=swarm", "service create "+serviceID, defaultRetryCount)

	t1 := daemonUnixTime(c)
	out, err = d.Cmd("service", "update", "--force", "--detach=false", "test")
	assert.NilError(c, err, out)

	// wait for service update start
	out = waitForEvent(c, d, t1, "-f scope=swarm", "service update "+serviceID, defaultRetryCount)
	assert.Assert(c, strings.Contains(out, "updatestate.new=updating"), out)
	// allow service update complete. This is a service with 1 instance
	time.Sleep(400 * time.Millisecond)
	out = waitForEvent(c, d, t1, "-f scope=swarm", "service update "+serviceID, defaultRetryCount)
	assert.Assert(c, strings.Contains(out, "updatestate.new=completed, updatestate.old=updating"), out)
	// scale service
	t2 := daemonUnixTime(c)
	out, err = d.Cmd("service", "scale", "test=3")
	assert.NilError(c, err, out)

	out = waitForEvent(c, d, t2, "-f scope=swarm", "service update "+serviceID, defaultRetryCount)
	assert.Assert(c, strings.Contains(out, "replicas.new=3, replicas.old=1"), out)
	// remove service
	t3 := daemonUnixTime(c)
	out, err = d.Cmd("service", "rm", "test")
	assert.NilError(c, err, out)

	waitForEvent(c, d, t3, "-f scope=swarm", "service remove "+serviceID, defaultRetryCount)
}

func (s *DockerSwarmSuite) TestSwarmClusterEventsNode(c *testing.T) {
	ctx := testutil.GetContext(c)
	d1 := s.AddDaemon(ctx, c, true, true)
	s.AddDaemon(ctx, c, true, true)
	d3 := s.AddDaemon(ctx, c, true, true)

	d3ID := d3.NodeID()
	waitForEvent(c, d1, "0", "-f scope=swarm", "node create "+d3ID, defaultRetryCount)

	t1 := daemonUnixTime(c)
	out, err := d1.Cmd("node", "update", "--availability=pause", d3ID)
	assert.NilError(c, err, out)

	// filter by type
	out = waitForEvent(c, d1, t1, "-f type=node", "node update "+d3ID, defaultRetryCount)
	assert.Assert(c, strings.Contains(out, "availability.new=pause, availability.old=active"), out)
	t2 := daemonUnixTime(c)
	out, err = d1.Cmd("node", "demote", d3ID)
	assert.NilError(c, err, out)

	waitForEvent(c, d1, t2, "-f type=node", "node update "+d3ID, defaultRetryCount)

	t3 := daemonUnixTime(c)
	out, err = d1.Cmd("node", "rm", "-f", d3ID)
	assert.NilError(c, err, out)

	// filter by scope
	waitForEvent(c, d1, t3, "-f scope=swarm", "node remove "+d3ID, defaultRetryCount)
}

func (s *DockerSwarmSuite) TestSwarmClusterEventsNetwork(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	// create a network
	out, err := d.Cmd("network", "create", "--attachable", "-d", "overlay", "foo")
	assert.NilError(c, err, out)
	networkID := strings.TrimSpace(out)

	waitForEvent(c, d, "0", "-f scope=swarm", "network create "+networkID, defaultRetryCount)

	// remove network
	t1 := daemonUnixTime(c)
	out, err = d.Cmd("network", "rm", "foo")
	assert.NilError(c, err, out)

	// filtered by network
	waitForEvent(c, d, t1, "-f type=network", "network remove "+networkID, defaultRetryCount)
}

func (s *DockerSwarmSuite) TestSwarmClusterEventsSecret(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	testName := "test_secret"
	id := d.CreateSecret(c, swarm.SecretSpec{
		Annotations: swarm.Annotations{
			Name: testName,
		},
		Data: []byte("TESTINGDATA"),
	})
	assert.Assert(c, id != "", "secrets: %s", id)

	waitForEvent(c, d, "0", "-f scope=swarm", "secret create "+id, defaultRetryCount)

	t1 := daemonUnixTime(c)
	d.DeleteSecret(c, id)
	// filtered by secret
	waitForEvent(c, d, t1, "-f type=secret", "secret remove "+id, defaultRetryCount)
}

func (s *DockerSwarmSuite) TestSwarmClusterEventsConfig(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	testName := "test_config"
	id := d.CreateConfig(c, swarm.ConfigSpec{
		Annotations: swarm.Annotations{
			Name: testName,
		},
		Data: []byte("TESTINGDATA"),
	})
	assert.Assert(c, id != "", "configs: %s", id)

	waitForEvent(c, d, "0", "-f scope=swarm", "config create "+id, defaultRetryCount)

	t1 := daemonUnixTime(c)
	d.DeleteConfig(c, id)
	// filtered by config
	waitForEvent(c, d, t1, "-f type=config", "config remove "+id, defaultRetryCount)
}

func getUnlockKey(d *daemon.Daemon, t *testing.T, autolockOutput string) string {
	unlockKey, err := d.Cmd("swarm", "unlock-key", "-q")
	assert.Assert(t, err == nil, unlockKey)
	unlockKey = strings.TrimSuffix(unlockKey, "\n")

	// Check that "docker swarm init --autolock" or "docker swarm update --autolock"
	// contains all the expected strings, including the unlock key
	assert.Assert(t, strings.Contains(autolockOutput, "docker swarm unlock"), autolockOutput)
	assert.Assert(t, strings.Contains(autolockOutput, unlockKey), autolockOutput)
	return unlockKey
}

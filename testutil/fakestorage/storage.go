package fakestorage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/build"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/testutil"
	"github.com/moby/moby/v2/testutil/environment"
	"github.com/moby/moby/v2/testutil/fakecontext"
	"github.com/moby/moby/v2/testutil/request"
	"gotest.tools/v3/assert"
)

var testEnv *environment.Execution

// Fake is a static file server. It might be running locally or remotely
// on test host.
type Fake interface {
	Close() error
	URL() string
	CtxDir() string
}

// SetTestEnvironment sets a static test environment
// TODO: decouple this package from environment
func SetTestEnvironment(env *environment.Execution) {
	testEnv = env
}

// New returns a static file server that is used as build context.
func New(t testing.TB, dir string, modifiers ...func(*fakecontext.Fake) error) Fake {
	t.Helper()
	if testEnv == nil {
		t.Fatal("fakstorage package requires SetTestEnvironment() to be called before use.")
	}
	ctx := fakecontext.New(t, dir, modifiers...)
	switch {
	case testEnv.IsRemoteDaemon() && strings.HasPrefix(request.DaemonHost(), "unix:///"):
		t.Skip("e2e run : daemon is remote but docker host points to a unix socket")
	case testEnv.IsLocalDaemon():
		return newLocalFakeStorage(ctx)
	default:
		return newRemoteFileServer(t, ctx, testEnv.APIClient())
	}
	return nil
}

// localFileStorage is a file storage on the running machine
type localFileStorage struct {
	*fakecontext.Fake
	*httptest.Server
}

func (s *localFileStorage) URL() string {
	return s.Server.URL
}

func (s *localFileStorage) CtxDir() string {
	return s.Fake.Dir
}

func (s *localFileStorage) Close() error {
	defer s.Server.Close()
	return s.Fake.Close()
}

func newLocalFakeStorage(ctx *fakecontext.Fake) *localFileStorage {
	handler := http.FileServer(http.Dir(ctx.Dir))
	server := httptest.NewServer(handler)
	return &localFileStorage{
		Fake:   ctx,
		Server: server,
	}
}

// remoteFileServer is a containerized static file server started on the remote
// testing machine to be used in URL-accepting docker build functionality.
type remoteFileServer struct {
	host      string // hostname/port web server is listening to on docker host e.g. 0.0.0.0:43712
	container string
	image     string
	client    client.APIClient
	ctx       *fakecontext.Fake
}

func (f *remoteFileServer) URL() string {
	u := url.URL{
		Scheme: "http",
		Host:   f.host,
	}
	return u.String()
}

func (f *remoteFileServer) CtxDir() string {
	return f.ctx.Dir
}

func (f *remoteFileServer) Close() error {
	defer func() {
		if f.ctx != nil {
			if err := f.ctx.Close(); err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "Error closing remote file server: closing context: %v\n", err)
			}
		}
		if f.image != "" {
			if _, err := f.client.ImageRemove(context.Background(), f.image, image.RemoveOptions{Force: true}); err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "Error closing remote file server: removing image: %v\n", err)
			}
		}
		if err := f.client.Close(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Error closing remote file server: closing client: %v\n", err)
		}
	}()
	if f.container == "" {
		return nil
	}
	return f.client.ContainerRemove(context.Background(), f.container, containertypes.RemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	})
}

func newRemoteFileServer(t testing.TB, ctx *fakecontext.Fake, c client.APIClient) *remoteFileServer {
	var (
		imgName   = fmt.Sprintf("fileserver-img-%s", strings.ToLower(testutil.GenerateRandomAlphaOnlyString(10)))
		container = fmt.Sprintf("fileserver-cnt-%s", strings.ToLower(testutil.GenerateRandomAlphaOnlyString(10)))
	)

	ensureHTTPServerImage(t)

	// Build the image
	if err := ctx.Add("Dockerfile", `FROM httpserver
COPY . /static`); err != nil {
		t.Fatal(err)
	}
	resp, err := c.ImageBuild(context.Background(), ctx.AsTarReader(t), build.ImageBuildOptions{
		NoCache: true,
		Tags:    []string{imgName},
	})
	assert.NilError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	assert.NilError(t, err)

	// Start the container
	b, err := c.ContainerCreate(context.Background(), &containertypes.Config{
		Image: imgName,
	}, &containertypes.HostConfig{}, nil, nil, container)
	assert.NilError(t, err)
	err = c.ContainerStart(context.Background(), b.ID, containertypes.StartOptions{})
	assert.NilError(t, err)

	// Find out the system assigned port
	i, err := c.ContainerInspect(context.Background(), b.ID)
	assert.NilError(t, err)
	ports, exists := i.NetworkSettings.Ports["80/tcp"]
	if !exists || len(ports) != 1 {
		t.Fatalf("unable to find port 80/tcp for %s", container)
	}
	host := ports[0].HostIP
	port := ports[0].HostPort

	return &remoteFileServer{
		container: container,
		image:     imgName,
		host:      fmt.Sprintf("%s:%s", host, port),
		ctx:       ctx,
		client:    c,
	}
}

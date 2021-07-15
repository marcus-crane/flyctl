package imgsrc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	dockerclient "github.com/docker/docker/client"
	"github.com/getsentry/sentry-go"
	"github.com/jpillora/backoff"
	"github.com/pkg/errors"
	"github.com/spf13/viper"
	"github.com/superfly/flyctl/api"
	"github.com/superfly/flyctl/flyctl"
	"github.com/superfly/flyctl/helpers"
	"github.com/superfly/flyctl/internal/monitor"
	"github.com/superfly/flyctl/internal/wireguard"
	"github.com/superfly/flyctl/pkg/iostreams"
	"github.com/superfly/flyctl/pkg/wg"
	"github.com/superfly/flyctl/terminal"
	"golang.org/x/sync/errgroup"
)

type dockerClientFactory struct {
	mode    DockerDaemonType
	buildFn func(ctx context.Context) (*dockerclient.Client, error)
}

func newDockerClientFactory(daemonType DockerDaemonType, apiClient *api.Client, appName string, streams *iostreams.IOStreams) *dockerClientFactory {
	if daemonType.AllowLocal() {
		terminal.Debug("trying local docker daemon")
		c, err := newLocalDockerClient()
		if c != nil && err == nil {
			return &dockerClientFactory{
				mode: DockerDaemonTypeLocal,
				buildFn: func(ctx context.Context) (*dockerclient.Client, error) {
					return c, nil
				},
			}
		} else if err != nil && !dockerclient.IsErrConnectionFailed(err) {
			terminal.Warn("Error connecting to local docker daemon:", err)
		} else {
			terminal.Debug("Local docker daemon unavailable")
		}
	}

	if daemonType.AllowRemote() {
		terminal.Debug("trying remote docker daemon")
		var cachedDocker *dockerclient.Client

		return &dockerClientFactory{
			mode: DockerDaemonTypeRemote,
			buildFn: func(ctx context.Context) (*dockerclient.Client, error) {
				if cachedDocker != nil {
					return cachedDocker, nil
				}
				c, err := newRemoteDockerClient(ctx, apiClient, appName, streams)
				if err != nil {
					return nil, err
				}
				cachedDocker = c
				return cachedDocker, nil
			},
		}
	}

	return &dockerClientFactory{
		mode: DockerDaemonTypeNone,
		buildFn: func(ctx context.Context) (*dockerclient.Client, error) {
			return nil, errors.New("no docker daemon available")
		},
	}
}

var unauthorizedError = errors.New("You are unauthorized to use this builder")

func isUnauthorized(err error) bool {
	return errors.Is(err, unauthorizedError)
}

func isRetyableError(err error) bool {
	return !isUnauthorized(err)
}

func NewDockerDaemonType(allowLocal, allowRemote bool) DockerDaemonType {
	daemonType := DockerDaemonTypeNone
	if allowLocal {
		daemonType = daemonType | DockerDaemonTypeLocal
	}
	if allowRemote {
		daemonType = daemonType | DockerDaemonTypeRemote
	}
	return daemonType
}

type DockerDaemonType int

const (
	DockerDaemonTypeLocal DockerDaemonType = 1 << iota
	DockerDaemonTypeRemote
	DockerDaemonTypeNone
)

func (t DockerDaemonType) AllowLocal() bool {
	return (t & DockerDaemonTypeLocal) != 0
}

func (t DockerDaemonType) AllowRemote() bool {
	return (t & DockerDaemonTypeRemote) != 0
}

func (t DockerDaemonType) AllowNone() bool {
	return (t & DockerDaemonTypeNone) != 0
}

func (t DockerDaemonType) IsLocal() bool {
	return t == DockerDaemonTypeLocal
}

func (t DockerDaemonType) IsRemote() bool {
	return t == DockerDaemonTypeRemote
}

func (t DockerDaemonType) IsNone() bool {
	return t == DockerDaemonTypeNone
}

func (t DockerDaemonType) IsAvailable() bool {
	return !t.IsNone()
}

func newLocalDockerClient() (*dockerclient.Client, error) {
	c, err := dockerclient.NewClientWithOpts(dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}

	if err := dockerclient.FromEnv(c); err != nil {
		return nil, err
	}

	if _, err = c.Ping(context.TODO()); err != nil {
		return nil, err
	}

	return c, nil
}

type remoteBuilderError struct {
	RemoteBuilderName string
	Err               error
}

func (e *remoteBuilderError) Error() string {
	return fmt.Sprintf("remote builder %s error %s", e.RemoteBuilderName, e.Err)
}

func newRemoteDockerClient(ctx context.Context, apiClient *api.Client, appName string, streams *iostreams.IOStreams) (*dockerclient.Client, error) {
	host, remoteBuilderAppName, err := remoteBuilderURL(apiClient, appName)
	if err != nil {
		return nil, err
	}

	terminal.Debugf("Remote Docker builder host: %s\n", host)

	if streams.IsInteractive() {
		streams.StartProgressIndicatorMsg(fmt.Sprintf("Waiting for remote builder %s... starting", remoteBuilderAppName))
	} else {
		fmt.Fprintf(streams.ErrOut, "Waiting for remote builder %s...\n", remoteBuilderAppName)
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	eg, errCtx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		defer streams.ChangeProgressIndicatorMsg(fmt.Sprintf("Waiting for remote builder %s... connecting", remoteBuilderAppName))

		if remoteBuilderAppName != "" {
			if err := monitor.WaitForRunningVM(errCtx, remoteBuilderAppName, apiClient); err != nil {
				return errors.Wrap(err, "Error waiting for remote builder app")
			}
		}
		return nil
	})

	clientCh := make(chan *dockerclient.Client, 1)

	eg.Go(func() error {
		opts := []dockerclient.Opt{
			dockerclient.WithAPIVersionNegotiation(),
			dockerclient.WithHost(host),
		}

		if os.Getenv("FLY_REMOTE_BUILDER_HOST_WG") == "" {
			app, err := apiClient.GetApp(appName)
			if err != nil {
				return errors.Wrap(err, "error fetching target app")
			}

			terminal.Debug("creating wireguard config for org ", app.Organization.Slug)
			state, err := wireguard.StateForOrg(apiClient, &app.Organization, "", "")
			if err != nil {
				return errors.Wrap(err, "error creating wireguard config")
			}

			terminal.Debugf("Establishing WireGuard connection (%s)\n", state.Name)

			tunnel, err := wg.Connect(*state.TunnelConfig())
			if err != nil {
				return errors.Wrap(err, "error establishing wireguard connection")
			}

			opts = append(opts, dockerclient.WithDialContext(tunnel.DialContext))
		} else {
			terminal.Debug("connecting to remote docker daemon over host wireguard tunnel")
		}

		client, err := dockerclient.NewClientWithOpts(opts...)
		if err != nil {
			return errors.Wrap(err, "Error creating docker client")
		}

		if err := waitForDaemon(errCtx, client); err != nil {
			return errors.Wrap(err, "error waiting for docker daemon")
		}

		clientCh <- client

		return nil
	})

	if err = eg.Wait(); err != nil {
		captureRemoteBuilderError(err, remoteBuilderAppName)

		return nil, err
	}

	if err := ctx.Err(); err != nil {
		captureRemoteBuilderError(err, remoteBuilderAppName)

		streams.StopProgressIndicator()
		if errors.Is(err, context.DeadlineExceeded) {
			terminal.Warnf("Remote builder did not start on time. Check remote builder logs with `flyctl logs -a %s`\n", remoteBuilderAppName)
			return nil, errors.New("remote builder app unavailable")
		}

		return nil, err
	}

	streams.StopProgressIndicatorMsg(fmt.Sprintf("Remote builder %s ready", remoteBuilderAppName))

	return <-clientCh, nil
}

func captureRemoteBuilderError(err error, builderAppName string) {
	if errors.Is(err, context.Canceled) {
		return
	}

	sentry.CaptureException(&remoteBuilderError{RemoteBuilderName: builderAppName, Err: err})
}

func remoteBuilderURL(apiClient *api.Client, appName string) (string, string, error) {
	if v := os.Getenv("FLY_REMOTE_BUILDER_HOST"); v != "" {
		return v, "", nil
	}

	_, app, err := apiClient.EnsureRemoteBuilderForApp(appName)
	if err != nil {
		return "", "", errors.Errorf("could not create remote builder: %v", err)
	}

	return "tcp://" + net.JoinHostPort(app.Name+".internal", "2375"), app.Name, nil
}

func waitForDaemon(ctx context.Context, client *dockerclient.Client) error {
	b := &backoff.Backoff{
		//These are the defaults
		Min:    200 * time.Millisecond,
		Max:    1 * time.Second,
		Factor: 1.2,
		Jitter: true,
	}

	consecutiveSuccesses := 0
	var healthyStart time.Time

	for {
		checkErr := make(chan error, 1)

		go func() {
			_, err := client.Ping(ctx)
			checkErr <- err
		}()

		select {
		case err := <-checkErr:
			if err == nil {
				if consecutiveSuccesses == 0 {
					// reset on the first success in a row so the next checks are a bit spaced out
					healthyStart = time.Now()
					b.Reset()
				}
				consecutiveSuccesses++

				if time.Since(healthyStart) > 1*time.Second {
					terminal.Debug("Remote builder is ready to build!")
					return nil
				}

				dur := b.Duration()
				terminal.Debugf("Remote builder available, but pinging again in %s to be sure\n", dur)
				time.Sleep(dur)
			} else {
				if !isRetyableError(err) {
					return err
				}
				consecutiveSuccesses = 0
				dur := b.Duration()
				terminal.Debugf("Remote builder unavailable, retrying in %s (err: %v)\n", dur, err)
				time.Sleep(dur)
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func clearDeploymentTags(ctx context.Context, docker *dockerclient.Client, tag string) error {
	filters := filters.NewArgs(filters.Arg("reference", tag))

	images, err := docker.ImageList(ctx, types.ImageListOptions{Filters: filters})
	if err != nil {
		return err
	}

	for _, image := range images {
		for _, tag := range image.RepoTags {
			_, err := docker.ImageRemove(ctx, tag, types.ImageRemoveOptions{PruneChildren: true})
			if err != nil {
				terminal.Debug("Error deleting image", err)
			}
		}
	}

	return nil
}

func registryAuth(token string) types.AuthConfig {
	return types.AuthConfig{
		Username:      "x",
		Password:      token,
		ServerAddress: "registry.fly.io",
	}
}

func authConfigs() map[string]types.AuthConfig {
	authConfigs := map[string]types.AuthConfig{}

	authConfigs["registry.fly.io"] = registryAuth(flyctl.GetAPIToken())

	dockerhubUsername := os.Getenv("DOCKER_HUB_USERNAME")
	dockerhubPassword := os.Getenv("DOCKER_HUB_PASSWORD")

	if dockerhubUsername != "" && dockerhubPassword != "" {
		cfg := types.AuthConfig{
			Username:      dockerhubUsername,
			Password:      dockerhubPassword,
			ServerAddress: "index.docker.io",
		}
		authConfigs["https://index.docker.io/v1/"] = cfg
	}

	return authConfigs
}

func flyRegistryAuth() string {
	accessToken := flyctl.GetAPIToken()
	authConfig := registryAuth(accessToken)
	encodedJSON, err := json.Marshal(authConfig)
	if err != nil {
		terminal.Warn("Error encoding fly registry credentials", err)
		return ""
	}
	return base64.URLEncoding.EncodeToString(encodedJSON)
}

func newDeploymentTag(appName string, label string) string {
	if tag := os.Getenv("FLY_IMAGE_REF"); tag != "" {
		return tag
	}

	if label == "" {
		label = fmt.Sprintf("deployment-%d", time.Now().Unix())
	}

	registry := viper.GetString(flyctl.ConfigRegistryHost)

	return fmt.Sprintf("%s/%s:%s", registry, appName, label)
}

func newCacheTag(appName string) string {
	registry := viper.GetString(flyctl.ConfigRegistryHost)

	return fmt.Sprintf("%s/%s:%s", registry, appName, "cache")
}

// resolveDockerfile - Resolve the location of the dockerfile, allowing for upper and lowercase naming
func resolveDockerfile(cwd string) string {
	dockerfilePath := filepath.Join(cwd, "Dockerfile")
	if helpers.FileExists(dockerfilePath) {
		return dockerfilePath
	}
	dockerfilePath = filepath.Join(cwd, "dockerfile")
	if helpers.FileExists(dockerfilePath) {
		return dockerfilePath
	}
	return ""
}

func EagerlyEnsureRemoteBuilder(apiClient *api.Client, orgSlug string) {
	// skip if local docker is available
	if _, err := newLocalDockerClient(); err == nil {
		return
	}

	org, err := apiClient.FindOrganizationBySlug(orgSlug)
	if err != nil {
		terminal.Debugf("error resolving organization for slug %s: %s", orgSlug, err)
		return
	}

	_, app, err := apiClient.EnsureRemoteBuilderForOrg(org.ID)
	if err != nil {
		terminal.Debugf("error ensuring remote builder for organization: %s", err)
		return
	}

	terminal.Debugf("remote builder %s is being prepared", app.Name)
}

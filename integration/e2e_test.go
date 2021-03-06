// +build end2end,docker

package integration

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/envoyproxy/xds-relay/internal/pkg/log"

	gcpcachev2 "github.com/envoyproxy/go-control-plane/pkg/cache/v2"
	gcpserverv2 "github.com/envoyproxy/go-control-plane/pkg/server/v2"
	gcpserverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	gcptest "github.com/envoyproxy/go-control-plane/pkg/test"
	gcpresourcev2 "github.com/envoyproxy/go-control-plane/pkg/test/resource/v2"
	gcptestv2 "github.com/envoyproxy/go-control-plane/pkg/test/v2"
	"github.com/envoyproxy/xds-relay/internal/app/server"
	yamlproto "github.com/envoyproxy/xds-relay/internal/pkg/util/yamlproto"
	aggregationv1 "github.com/envoyproxy/xds-relay/pkg/api/aggregation/v1"
	bootstrapv1 "github.com/envoyproxy/xds-relay/pkg/api/bootstrap/v1"
	"github.com/onsi/gomega"
)

var testLogger = log.MockLogger.Named("e2e")

func TestMain(m *testing.M) {
	// We force a 1 second sleep before running a test to let the OS close any lingering socket from previous
	// tests.
	time.Sleep(1 * time.Second)
	code := m.Run()
	os.Exit(code)
}

func TestSnapshotCacheSingleEnvoyAndXdsRelayServer(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Golang does not offer a cross-platform safe way of killing child processes, so we skip these tests if not on linux.")
	}

	g := gomega.NewWithT(t)

	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	// Test parameters
	const (
		managementServerPort uint = 18000 // gRPC management server port
		httpServicePort      uint = 18080 // upstream HTTP/1.1 service that Envoy wll call
		envoyListenerPort    uint = 9000  // initial port for the Envoy listeners generated by the snapshot cache
		nClusters                 = 7
		nListeners                = 9
		nUpdates                  = 4
		keyerConfiguration        = "./testdata/keyer_configuration_e2e.yaml"
		xdsRelayBootstrap         = "./testdata/bootstrap_configuration_e2e.yaml"
		envoyBootstrap            = "./testdata/envoy_bootstrap.yaml"
	)

	// We run a service that returns the string "Hi, there!" locally and expose it through envoy.
	// This is the service that Envoy will make requests to.
	go gcptest.RunHTTP(ctx, httpServicePort)

	// Mimic a management server using go-control-plane's snapshot cache.
	managementServer, signal := startSnapshotCache(ctx, managementServerPort)

	// Start xds-relay server.
	startXdsRelayServer(ctx, cancelFunc, xdsRelayBootstrap, keyerConfiguration)

	// Start envoy and return a bytes buffer containing the envoy logs.
	envoyLogsBuffer := startEnvoy(ctx, envoyBootstrap, signal)

	// Initial cached snapshot configuration.
	snapshotConfig := gcpresourcev2.TestSnapshot{
		Xds:              "xds",
		UpstreamPort:     uint32(httpServicePort),
		BasePort:         uint32(envoyListenerPort),
		NumClusters:      nClusters,
		NumHTTPListeners: nListeners,
	}

	for i := 0; i < nUpdates; i++ {
		// Bumping the snapshot version mimics new management server configuration.
		snapshotConfig.Version = fmt.Sprintf("v%d", i)
		testLogger.Info(ctx, "updating snapshots to version: %v", snapshotConfig.Version)

		snapshot := snapshotConfig.Generate()
		if err := snapshot.Consistent(); err != nil {
			testLogger.Fatal(ctx, "snapshot inconsistency: %+v", snapshot)
		}
		err := managementServer.SetSnapshot("envoy-1", snapshot)
		if err != nil {
			testLogger.Fatal(ctx, "set snapshot error %q for %+v", err, snapshot)
		}

		g.Eventually(func() (int, int) {
			ok, failed := callLocalService(envoyListenerPort, nListeners)
			testLogger.Info(ctx, "asserting envoy listeners configured: ok %v, failed %v", ok, failed)
			return ok, failed
		}, 1*time.Second, 100*time.Millisecond).Should(gomega.Equal(nListeners))
	}

	// TODO(https://github.com/envoyproxy/xds-relay/issues/66): figure out a way to only only copy
	// envoy logs in case of failures.
	testLogger.With("envoy logs", envoyLogsBuffer.String()).Debug(ctx, "captured envoy logs")
}

func startSnapshotCache(ctx context.Context, port uint) (gcpcachev2.SnapshotCache, chan struct{}) {
	// Create a cache
	signal := make(chan struct{})
	cbv2 := &gcptestv2.Callbacks{Signal: signal}
	configv2 := gcpcachev2.NewSnapshotCache(false, gcpcachev2.IDHash{}, gcpLogger{logger: testLogger.Named("snapshot")})
	srv2 := gcpserverv2.NewServer(ctx, configv2, cbv2)
	// We don't have support for v3 yet, but this is left here in preparation for the eventual
	// inclusion of v3 resources.
	srv3 := gcpserverv3.NewServer(ctx, nil, nil)

	// Start up a gRPC-based management server.
	go gcptest.RunManagementServer(ctx, srv2, srv3, port)

	return configv2, signal
}

func startXdsRelayServer(ctx context.Context, cancel context.CancelFunc, bootstrapConfigFilePath string,
	keyerConfigurationFilePath string) {
	bootstrapConfigFileContent, err := ioutil.ReadFile(bootstrapConfigFilePath)
	if err != nil {
		testLogger.Fatal(ctx, "failed to read bootstrap config file: ", err)
	}
	var bootstrapConfig bootstrapv1.Bootstrap
	err = yamlproto.FromYAMLToBootstrapConfiguration(string(bootstrapConfigFileContent), &bootstrapConfig)
	if err != nil {
		testLogger.Fatal(ctx, "failed to translate bootstrap config: ", err)
	}

	aggregationRulesFileContent, err := ioutil.ReadFile(keyerConfigurationFilePath)
	if err != nil {
		testLogger.Fatal(ctx, "failed to read aggregation rules file: ", err)
	}
	var aggregationRulesConfig aggregationv1.KeyerConfiguration
	err = yamlproto.FromYAMLToKeyerConfiguration(string(aggregationRulesFileContent), &aggregationRulesConfig)
	if err != nil {
		testLogger.Fatal(ctx, "failed to translate aggregation rules: ", err)
	}
	go server.RunWithContext(ctx, cancel, &bootstrapConfig, &aggregationRulesConfig, "debug", "serve")
}

func startEnvoy(ctx context.Context, bootstrapFilePath string, signal chan struct{}) bytes.Buffer {
	envoyCmd := exec.CommandContext(ctx, "envoy", "-c", bootstrapFilePath, "--log-level", "debug")
	var b bytes.Buffer
	envoyCmd.Stdout = &b
	envoyCmd.Stderr = &b
	// Golang does not offer a portable solution to kill all child processes upon parent exit, so we rely on
	// this linuxism to send a SIGKILL to the envoy process (and its child sub-processes) when the parent (the
	// test) exits. More information in http://man7.org/linux/man-pages/man2/prctl.2.html
	envoyCmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	envoyCmd.Start()

	testLogger.Info(ctx, "waiting for upstream cluster to send the first response ...")
	select {
	case <-signal:
		break
	case <-time.After(1 * time.Minute):
		testLogger.Info(ctx, "envoy logs: \n%s", b.String())
		testLogger.Fatal(ctx, "timeout waiting for upstream cluster to send the first response")
	}

	return b
}

func callLocalService(port uint, nListeners int) (int, int) {
	ok, failed := 0, 0
	ch := make(chan error, nListeners)

	// spawn requests
	for i := 0; i < nListeners; i++ {
		go func(i int) {
			client := http.Client{
				Timeout:   100 * time.Millisecond,
				Transport: &http.Transport{},
			}
			req, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d", port+uint(i)))
			if err != nil {
				ch <- err
				return
			}
			defer req.Body.Close()
			body, err := ioutil.ReadAll(req.Body)
			if err != nil {
				ch <- err
				return
			}
			if string(body) != gcptest.Hello {
				ch <- fmt.Errorf("expected envoy response: %q, got: %q", gcptest.Hello, string(body))
				return
			}
			ch <- nil
		}(i)
	}

	for {
		out := <-ch
		if out == nil {
			ok++
		} else {
			testLogger.With("err", out).Info(context.Background(), "envoy request error")
			failed++
		}
		if ok+failed == nListeners {
			return ok, failed
		}
	}
}

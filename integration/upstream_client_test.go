// +build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gcpcachev2 "github.com/envoyproxy/go-control-plane/pkg/cache/v2"
	gcpserverv2 "github.com/envoyproxy/go-control-plane/pkg/server/v2"
	gcpserverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	gcptest "github.com/envoyproxy/go-control-plane/pkg/test"
	resourcev2 "github.com/envoyproxy/go-control-plane/pkg/test/resource/v2"
	gcptestv2 "github.com/envoyproxy/go-control-plane/pkg/test/v2"
	"github.com/stretchr/testify/assert"

	v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	corev2 "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	"github.com/envoyproxy/xds-relay/internal/app/upstream"
	"github.com/envoyproxy/xds-relay/internal/pkg/log"
)

const (
	nodeID           = "node-id"
	originServerPort = 19001
	updates          = 1
)

func TestXdsClientGetsIncrementalResponsesFromUpstreamServer(t *testing.T) {
	updates := 2
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	snapshotsv2, configv2 := createSnapshotCache(updates, log.MockLogger)
	cb := gcptestv2.Callbacks{Signal: make(chan struct{})}
	respCh, _, err := setup(ctx, log.MockLogger, snapshotsv2, configv2, &cb)
	if err != nil {
		assert.Fail(t, "Setup failed: %s", err.Error())
		return
	}

	var wg sync.WaitGroup
	wg.Add(updates)
	version := 0
	go func() {
		for {
			select {
			case r, more := <-respCh:
				if !more {
					return
				}
				assert.Equal(t, r.VersionInfo, "v"+strconv.Itoa(version))
				version++
				wg.Done()
			}
		}
	}()

	sendResponses(ctx, log.MockLogger, updates, snapshotsv2, configv2)
	wg.Wait()

	timeoutCtx, timeoutCtxCancel := context.WithTimeout(ctx, 10*time.Second)
	defer timeoutCtxCancel()
	select {
	case <-timeoutCtx.Done():
		assert.Fail(t, "request count did not match")
	case <-time.After(1 * time.Second):
		// There should be one extra waiting request for the watch
		assert.Equal(t, updates+1, cb.Requests)
		break
	}
}

func TestXdsClientShutdownShouldCloseTheResponseChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	snapshotsv2, configv2 := createSnapshotCache(updates, log.MockLogger)
	cb := gcptestv2.Callbacks{Signal: make(chan struct{})}
	respCh, shutdown, err := setup(ctx, log.MockLogger, snapshotsv2, configv2, &cb)
	if err != nil {
		assert.Fail(t, "Setup failed: %s", err.Error())
		return
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		select {
		case _, more := <-respCh:
			if !more {
				wg.Done()
			}
		}
	}()

	sendResponses(ctx, log.MockLogger, updates, snapshotsv2, configv2)
	shutdown()
	wg.Wait()
}

func TestServerShutdownShouldCloseResponseChannel(t *testing.T) {
	serverCtx, cancel := context.WithCancel(context.Background())

	snapshotsv2, configv2 := createSnapshotCache(updates, log.MockLogger)
	cb := gcptestv2.Callbacks{Signal: make(chan struct{})}
	respCh, _, err := setup(serverCtx, log.MockLogger, snapshotsv2, configv2, &cb)
	if err != nil {
		assert.Fail(t, "Setup failed: %s", err.Error())
		cancel()
		return
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		for {
			select {
			case _, more := <-respCh:
				if !more {
					wg.Done()
					return
				}
			}
		}
	}()

	sendResponses(serverCtx, log.MockLogger, updates, snapshotsv2, configv2)
	cancel()
	wg.Wait()
}

func TestClientContextCancellationShouldCloseAllResponseChannels(t *testing.T) {
	serverCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	snapshotsv2, configv2 := createSnapshotCache(updates, log.MockLogger)
	cb := gcptestv2.Callbacks{Signal: make(chan struct{})}
	_, _, err := setup(serverCtx, log.MockLogger, snapshotsv2, configv2, &cb)
	if err != nil {
		assert.Fail(t, "Setup failed: %s", err.Error())
		return
	}

	clientCtx, clientCancel := context.WithCancel(context.Background())
	client, err := upstream.New(
		clientCtx,
		strings.Join([]string{"127.0.0.1", strconv.Itoa(originServerPort)}, ":"),
		upstream.CallOptions{Timeout: time.Minute},
		log.MockLogger)
	respCh1, _, _ := client.OpenStream(v2.DiscoveryRequest{
		TypeUrl: upstream.ClusterTypeURL,
		Node: &corev2.Node{
			Id: nodeID,
		},
	})
	respCh2, _, _ := client.OpenStream(v2.DiscoveryRequest{
		TypeUrl: upstream.ClusterTypeURL,
		Node: &corev2.Node{
			Id: nodeID,
		},
	})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		select {
		case _, more := <-respCh1:
			if !more {
				wg.Done()
				break
			}
		}

		select {
		case _, more := <-respCh2:
			if !more {
				wg.Done()
				break
			}
		}
	}()

	sendResponses(serverCtx, log.MockLogger, updates, snapshotsv2, configv2)
	clientCancel()
	wg.Wait()
}

func setup(
	ctx context.Context,
	logger log.Logger,
	snapshotv2 resourcev2.TestSnapshot,
	configv2 gcpcachev2.SnapshotCache,
	cb *gcptestv2.Callbacks) (<-chan *v2.DiscoveryResponse, func(), error) {
	srv2 := gcpserverv2.NewServer(ctx, configv2, cb)
	srv3 := gcpserverv3.NewServer(ctx, nil, nil)

	// Start the origin server
	go gcptest.RunManagementServer(ctx, srv2, srv3, originServerPort)

	client, err := upstream.New(
		context.Background(),
		strings.Join([]string{"127.0.0.1", strconv.Itoa(originServerPort)}, ":"),
		upstream.CallOptions{Timeout: time.Minute},
		logger)
	if err != nil {
		logger.Error(ctx, "NewClient failed %s", err.Error())
		return nil, nil, err
	}

	respCh, shutdown, err := client.OpenStream(v2.DiscoveryRequest{
		TypeUrl: upstream.ClusterTypeURL,
		Node: &corev2.Node{
			Id: nodeID,
		},
	})

	if err != nil {
		logger.Error(ctx, "Open stream failed %s", err.Error())
		return nil, nil, err
	}

	select {
	case <-cb.Signal:
		break
	case <-time.After(10 * time.Second):
		logger.Error(ctx, "timeout waiting for the first request")
		return nil, nil, fmt.Errorf("timeout waiting for the first request")
	}

	return respCh, shutdown, nil
}

func sendResponses(
	ctx context.Context,
	logger log.Logger,
	updates int,
	snapshot resourcev2.TestSnapshot,
	cache gcpcachev2.SnapshotCache,
) {
	for i := 0; i < updates; i++ {
		snapshot.Version = fmt.Sprintf("v%d", i)
		newSnapshot := snapshot.Generate()
		if err := newSnapshot.Consistent(); err != nil {
			logger.Error(ctx, "snapshot inconsistency: %+v\n", newSnapshot)
		}
		err := cache.SetSnapshot(nodeID, newSnapshot)
		if err != nil {
			logger.Error(ctx, "snapshot error %q for %+v\n", err, newSnapshot)
			os.Exit(1)
		}
	}
}

func createSnapshotCache(updates int, logger log.Logger) (resourcev2.TestSnapshot, gcpcachev2.SnapshotCache) {
	return resourcev2.TestSnapshot{
		Xds:          "xds",
		UpstreamPort: 18080,
		BasePort:     9000,
		NumClusters:  updates,
	}, gcpcachev2.NewSnapshotCache(false, gcpcachev2.IDHash{}, gcpLogger{logger: logger})
}

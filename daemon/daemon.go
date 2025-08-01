// FIXME(thaJeztah): remove once we are a module; the go:build directive prevents go from downgrading language version to go1.16:
//go:build go1.23

// Package daemon exposes the functions that occur on the host server
// that the Docker daemon is running.
//
// In implementing the various functions of the daemon, there is often
// a method-specific struct for configuring the runtime behavior.
package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/dialer"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/distribution/reference"
	dist "github.com/docker/distribution"
	"github.com/docker/docker/api/types/backend"
	containertypes "github.com/docker/docker/api/types/container"
	imagetypes "github.com/docker/docker/api/types/image"
	networktypes "github.com/docker/docker/api/types/network"
	registrytypes "github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/api/types/swarm"
	volumetypes "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/daemon/builder"
	executorpkg "github.com/docker/docker/daemon/cluster/executor"
	"github.com/docker/docker/daemon/config"
	"github.com/docker/docker/daemon/container"
	ctrd "github.com/docker/docker/daemon/containerd"
	"github.com/docker/docker/daemon/events"
	_ "github.com/docker/docker/daemon/graphdriver/register" // register graph drivers
	"github.com/docker/docker/daemon/images"
	libcontainerdtypes "github.com/docker/docker/daemon/internal/libcontainerd/types"
	"github.com/docker/docker/daemon/internal/metrics"
	pluginexec "github.com/docker/docker/daemon/internal/plugin/executor/containerd"
	"github.com/docker/docker/daemon/libnetwork"
	"github.com/docker/docker/daemon/libnetwork/cluster"
	nwconfig "github.com/docker/docker/daemon/libnetwork/config"
	"github.com/docker/docker/daemon/libnetwork/ipamutils"
	"github.com/docker/docker/daemon/libnetwork/ipbits"
	dlogger "github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/daemon/network"
	"github.com/docker/docker/daemon/pkg/plugin"
	"github.com/docker/docker/daemon/snapshotter"
	"github.com/docker/docker/daemon/stats"
	"github.com/docker/docker/distribution"
	dmetadata "github.com/docker/docker/distribution/metadata"
	"github.com/docker/docker/dockerversion"
	"github.com/docker/docker/image"
	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/authorization"
	"github.com/docker/docker/pkg/fileutils"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/plugingetter"
	"github.com/docker/docker/pkg/sysinfo"
	refstore "github.com/docker/docker/reference"
	"github.com/docker/docker/registry"
	volumesservice "github.com/docker/docker/volume/service"
	"github.com/moby/buildkit/util/grpcerrors"
	"github.com/moby/buildkit/util/tracing"
	"github.com/moby/locker"
	"github.com/moby/sys/user"
	"github.com/moby/sys/userns"
	"github.com/pkg/errors"
	bolt "go.etcd.io/bbolt"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
	"resenje.org/singleflight"
	"tags.cncf.io/container-device-interface/pkg/cdi"
)

type configStore struct {
	config.Config

	Runtimes runtimes
}

// Daemon holds information about the Docker daemon.
type Daemon struct {
	id                string
	repository        string
	containers        container.Store
	containersReplica *container.ViewDB
	execCommands      *container.ExecStore
	imageService      ImageService
	configStore       atomic.Pointer[configStore]
	configReload      sync.Mutex
	statsCollector    *stats.Collector
	defaultLogConfig  containertypes.LogConfig
	registryService   *registry.Service
	EventsService     *events.Events
	netController     *libnetwork.Controller
	volumes           *volumesservice.VolumesService
	root              string
	sysInfoOnce       sync.Once
	sysInfo           *sysinfo.SysInfo
	shutdown          bool
	idMapping         user.IdentityMapping
	PluginStore       *plugin.Store // TODO: remove
	pluginManager     *plugin.Manager
	linkIndex         *linkIndex
	containerdClient  *containerd.Client
	containerd        libcontainerdtypes.Client
	defaultIsolation  containertypes.Isolation // Default isolation mode on Windows
	clusterProvider   cluster.Provider
	cluster           Cluster
	genericResources  []swarm.GenericResource
	ReferenceStore    refstore.Store

	machineMemory uint64

	seccompProfile     []byte
	seccompProfilePath string

	usageContainers singleflight.Group[struct{}, *containertypes.DiskUsage]
	usageImages     singleflight.Group[struct{}, []*imagetypes.Summary]
	usageVolumes    singleflight.Group[struct{}, *volumetypes.DiskUsage]
	usageLayer      singleflight.Group[struct{}, int64]

	pruneRunning atomic.Bool
	hosts        map[string]bool // hosts stores the addresses the daemon is listening on
	startupDone  chan struct{}

	attachmentStore       network.AttachmentStore
	attachableNetworkLock *locker.Locker

	// This is used for Windows which doesn't currently support running on containerd
	// It stores metadata for the content store (used for manifest caching)
	// This needs to be closed on daemon exit
	mdDB *bolt.DB

	usesSnapshotter bool

	CDICache *cdi.Cache
}

// ID returns the daemon id
func (daemon *Daemon) ID() string {
	return daemon.id
}

// StoreHosts stores the addresses the daemon is listening on
func (daemon *Daemon) StoreHosts(hosts []string) {
	if daemon.hosts == nil {
		daemon.hosts = make(map[string]bool)
	}
	for _, h := range hosts {
		daemon.hosts[h] = true
	}
}

// config returns an immutable snapshot of the current daemon configuration.
// Multiple calls to this function will return the same pointer until the
// configuration is reloaded so callers must take care not to modify the
// returned value.
//
// To ensure that the configuration used remains consistent throughout the
// lifetime of an operation, the configuration pointer should be passed down the
// call stack, like one would a [context.Context] value. Only the entrypoints
// for operations, the outermost functions, should call this function.
func (daemon *Daemon) config() *configStore {
	cfg := daemon.configStore.Load()
	if cfg == nil {
		return &configStore{}
	}
	return cfg
}

// Config returns daemon's config.
func (daemon *Daemon) Config() config.Config {
	return daemon.config().Config
}

// HasExperimental returns whether the experimental features of the daemon are enabled or not
func (daemon *Daemon) HasExperimental() bool {
	return daemon.config().Experimental
}

// Features returns the features map from configStore
func (daemon *Daemon) Features() map[string]bool {
	return daemon.config().Features
}

// UsesSnapshotter returns true if feature flag to use containerd snapshotter is enabled
func (daemon *Daemon) UsesSnapshotter() bool {
	return daemon.usesSnapshotter
}

func (daemon *Daemon) restore(cfg *configStore) error {
	var mapLock sync.Mutex
	containers := make(map[string]*container.Container)

	log.G(context.TODO()).Info("Loading containers: start.")

	dir, err := os.ReadDir(daemon.repository)
	if err != nil {
		return err
	}

	// parallelLimit is the maximum number of parallel startup jobs that we
	// allow (this is the limited used for all startup semaphores). The multiplier
	// (128) was chosen after some fairly significant benchmarking -- don't change
	// it unless you've tested it significantly (this value is adjusted if
	// RLIMIT_NOFILE is small to avoid EMFILE).
	parallelLimit := adjustParallelLimit(len(dir), 128*runtime.NumCPU())

	// Re-used for all parallel startup jobs.
	var group sync.WaitGroup
	sem := semaphore.NewWeighted(int64(parallelLimit))

	for _, v := range dir {
		group.Add(1)
		go func(id string) {
			defer group.Done()
			_ = sem.Acquire(context.Background(), 1)
			defer sem.Release(1)

			logger := log.G(context.TODO()).WithField("container", id)

			c, err := daemon.load(id)
			if err != nil {
				logger.WithError(err).Error("failed to load container")
				return
			}
			if c.Driver != daemon.imageService.StorageDriver() {
				// Ignore the container if it wasn't created with the current storage-driver
				logger.Debugf("not restoring container because it was created with another storage driver (%s)", c.Driver)
				return
			}
			rwlayer, err := daemon.imageService.GetLayerByID(c.ID)
			if err != nil {
				logger.WithError(err).Error("failed to load container mount")
				return
			}
			c.RWLayer = rwlayer
			logger.WithFields(log.Fields{
				"running": c.IsRunning(),
				"paused":  c.IsPaused(),
			}).Debug("loaded container")

			mapLock.Lock()
			containers[c.ID] = c
			mapLock.Unlock()
		}(v.Name())
	}
	group.Wait()

	removeContainers := make(map[string]*container.Container)
	restartContainers := make(map[*container.Container]chan struct{})
	activeSandboxes := make(map[string]any)

	for _, c := range containers {
		group.Add(1)
		go func(c *container.Container) {
			defer group.Done()
			_ = sem.Acquire(context.Background(), 1)
			defer sem.Release(1)

			logger := log.G(context.TODO()).WithField("container", c.ID)

			if err := daemon.registerName(c); err != nil {
				logger.WithError(err).Errorf("failed to register container name: %s", c.Name)
				mapLock.Lock()
				delete(containers, c.ID)
				mapLock.Unlock()
				return
			}
			if err := daemon.register(context.TODO(), c); err != nil {
				logger.WithError(err).Error("failed to register container")
				mapLock.Lock()
				delete(containers, c.ID)
				mapLock.Unlock()
				return
			}
		}(c)
	}
	group.Wait()

	for _, c := range containers {
		group.Add(1)
		go func(c *container.Container) {
			defer group.Done()
			_ = sem.Acquire(context.Background(), 1)
			defer sem.Release(1)

			baseLogger := log.G(context.TODO()).WithField("container", c.ID)

			if c.HostConfig != nil {
				// Migrate containers that don't have the default ("no") restart-policy set.
				// The RestartPolicy.Name field may be empty for containers that were
				// created with versions before v25.0.0.
				//
				// We also need to set the MaximumRetryCount to 0, to prevent
				// validation from failing (MaximumRetryCount is not allowed if
				// no restart-policy ("none") is set).
				if c.HostConfig.RestartPolicy.Name == "" {
					baseLogger.Debug("migrated restart-policy")
					c.HostConfig.RestartPolicy.Name = containertypes.RestartPolicyDisabled
					c.HostConfig.RestartPolicy.MaximumRetryCount = 0
				}

				// Normalize the "default" network mode into the network mode
				// it aliases ("bridge on Linux and "nat" on Windows). This is
				// also done by the container router, for new containers. But
				// we need to do it here too to handle containers that were
				// created prior to v26.0.
				//
				// TODO(aker): remove this migration code once the next LTM version of MCR is released.
				if c.HostConfig.NetworkMode.IsDefault() {
					c.HostConfig.NetworkMode = network.DefaultNetwork
					if nw, ok := c.NetworkSettings.Networks[networktypes.NetworkDefault]; ok {
						c.NetworkSettings.Networks[c.HostConfig.NetworkMode.NetworkName()] = nw
						delete(c.NetworkSettings.Networks, networktypes.NetworkDefault)
					}
				}

				// The logger option 'fluentd-async-connect' has been
				// deprecated in v20.10 in favor of 'fluentd-async', and
				// removed in v28.0.
				// TODO(aker): remove this migration once the next LTS version of MCR is released.
				if v, ok := c.HostConfig.LogConfig.Config["fluentd-async-connect"]; ok {
					if _, ok := c.HostConfig.LogConfig.Config["fluentd-async"]; !ok {
						c.HostConfig.LogConfig.Config["fluentd-async"] = v
					}
					delete(c.HostConfig.LogConfig.Config, "fluentd-async-connect")
				}
			}

			if err := daemon.checkpointAndSave(c); err != nil {
				baseLogger.WithError(err).Error("failed to save migrated container config to disk")
			}

			daemon.setStateCounter(c)

			logger := func(c *container.Container) *log.Entry {
				return baseLogger.WithFields(log.Fields{
					"running":    c.IsRunning(),
					"paused":     c.IsPaused(),
					"restarting": c.IsRestarting(),
				})
			}

			logger(c).Debug("restoring container")

			var es *containerd.ExitStatus

			if err := c.RestoreTask(context.Background(), daemon.containerd); err != nil && !cerrdefs.IsNotFound(err) {
				logger(c).WithError(err).Error("failed to restore container with containerd")
				return
			}

			alive := false
			status := containerd.Unknown
			if tsk, ok := c.Task(); ok {
				s, err := tsk.Status(context.Background())
				if err != nil {
					logger(c).WithError(err).Error("failed to get task status")
				} else {
					status = s.Status
					alive = status != containerd.Stopped
					if !alive {
						logger(c).Debug("cleaning up dead container process")
						es, err = tsk.Delete(context.Background())
						if err != nil && !cerrdefs.IsNotFound(err) {
							logger(c).WithError(err).Error("failed to delete task from containerd")
							return
						}
					} else if !cfg.LiveRestoreEnabled {
						logger(c).Debug("shutting down container considered alive by containerd")
						if err := daemon.shutdownContainer(c); err != nil && !cerrdefs.IsNotFound(err) {
							baseLogger.WithError(err).Error("error shutting down container")
							return
						}
						status = containerd.Stopped
						alive = false
						c.ResetRestartManager(false)
					}
				}
			}
			// If the containerd task for the container was not found, docker's view of the
			// container state will be updated accordingly via SetStopped further down.

			if c.IsRunning() || c.IsPaused() {
				logger(c).Debug("syncing container on disk state with real state")

				c.RestartManager().Cancel() // manually start containers because some need to wait for swarm networking

				switch {
				case c.IsPaused() && alive:
					logger(c).WithField("state", status).Info("restored container paused")
					switch status {
					case containerd.Paused, containerd.Pausing:
						// nothing to do
					case containerd.Unknown, containerd.Stopped, "":
						baseLogger.WithField("status", status).Error("unexpected status for paused container during restore")
					default:
						// running
						c.Lock()
						c.Paused = false
						daemon.setStateCounter(c)
						daemon.initHealthMonitor(c)
						if err := c.CheckpointTo(context.TODO(), daemon.containersReplica); err != nil {
							baseLogger.WithError(err).Error("failed to update paused container state")
						}
						c.Unlock()
					}
				case !c.IsPaused() && alive:
					logger(c).Debug("restoring healthcheck")
					c.Lock()
					daemon.initHealthMonitor(c)
					c.Unlock()
				}

				if !alive {
					logger(c).Debug("setting stopped state")
					c.Lock()
					var ces container.ExitStatus
					if es != nil {
						ces.ExitCode = int(es.ExitCode())
						ces.ExitedAt = es.ExitTime()
					} else {
						ces.ExitCode = 255
					}
					c.SetStopped(&ces)
					daemon.Cleanup(context.TODO(), c)
					if err := c.CheckpointTo(context.TODO(), daemon.containersReplica); err != nil {
						baseLogger.WithError(err).Error("failed to update stopped container state")
					}
					c.Unlock()
					logger(c).Debug("set stopped state")
				}

				// we call Mount and then Unmount to get BaseFs of the container
				if err := daemon.Mount(c); err != nil {
					// The mount is unlikely to fail. However, in case mount fails
					// the container should be allowed to restore here. Some functionalities
					// (like docker exec -u user) might be missing but container is able to be
					// stopped/restarted/removed.
					// See #29365 for related information.
					// The error is only logged here.
					logger(c).WithError(err).Warn("failed to mount container to get BaseFs path")
				} else {
					if err := daemon.Unmount(c); err != nil {
						logger(c).WithError(err).Warn("failed to umount container to get BaseFs path")
					}
				}

				c.ResetRestartManager(false)
				if !c.HostConfig.NetworkMode.IsContainer() && c.IsRunning() {
					options, err := buildSandboxOptions(&cfg.Config, c)
					if err != nil {
						logger(c).WithError(err).Warn("failed to build sandbox option to restore container")
					}
					mapLock.Lock()
					activeSandboxes[c.NetworkSettings.SandboxID] = options
					mapLock.Unlock()
				}
			}

			// get list of containers we need to restart

			// Do not autostart containers which
			// has endpoints in a swarm scope
			// network yet since the cluster is
			// not initialized yet. We will start
			// it after the cluster is
			// initialized.
			if cfg.AutoRestart && c.ShouldRestart() && !c.NetworkSettings.HasSwarmEndpoint && c.HasBeenStartedBefore {
				mapLock.Lock()
				restartContainers[c] = make(chan struct{})
				mapLock.Unlock()
			} else if c.HostConfig != nil && c.HostConfig.AutoRemove {
				// Remove the container if live-restore is disabled or if the container has already exited.
				if !cfg.LiveRestoreEnabled || !alive {
					mapLock.Lock()
					removeContainers[c.ID] = c
					mapLock.Unlock()
				}
			}

			c.Lock()
			// TODO(thaJeztah): we no longer persist RemovalInProgress on disk, so this code is likely redundant; see https://github.com/moby/moby/pull/49968
			if c.RemovalInProgress {
				// We probably crashed in the middle of a removal, reset
				// the flag.
				//
				// We DO NOT remove the container here as we do not
				// know if the user had requested for either the
				// associated volumes, network links or both to also
				// be removed. So we put the container in the "dead"
				// state and leave further processing up to them.
				c.RemovalInProgress = false
				c.Dead = true
				if err := c.CheckpointTo(context.TODO(), daemon.containersReplica); err != nil {
					baseLogger.WithError(err).Error("failed to update RemovalInProgress container state")
				} else {
					baseLogger.Debugf("reset RemovalInProgress state for container")
				}
			}
			c.Unlock()
			logger(c).Debug("done restoring container")
		}(c)
	}
	group.Wait()

	// Initialize the network controller and configure network settings.
	//
	// Note that we cannot initialize the network controller earlier, as it
	// needs to know if there's active sandboxes (running containers).
	if err = daemon.initNetworkController(&cfg.Config, activeSandboxes); err != nil {
		return fmt.Errorf("Error initializing network controller: %v", err)
	}

	for _, c := range containers {
		sb, err := daemon.netController.SandboxByID(c.NetworkSettings.SandboxID)
		if err != nil {
			// If this container's sandbox doesn't exist anymore, that's because
			// the netController gc'ed it during its initialization. In that case,
			// we need to clear all the network-related state carried by that
			// container.
			daemon.releaseNetwork(context.WithoutCancel(context.TODO()), c)
			continue
		}
		// If port-mapping failed during live-restore of a container, perhaps because
		// a host port that was previously mapped to a container is now in-use by some
		// other process - ports will not be mapped for the restored container, but it
		// will be running. Replace the restored mappings in NetworkSettings with the
		// current state so that the problem is visible in 'inspect'.
		c.NetworkSettings.Ports = getPortMapInfo(sb)
	}

	// Now that all the containers are registered, register the links
	for _, c := range containers {
		group.Add(1)
		go func(c *container.Container) {
			_ = sem.Acquire(context.Background(), 1)

			if err := daemon.registerLinks(c, c.HostConfig); err != nil {
				log.G(context.TODO()).WithField("container", c.ID).WithError(err).Error("failed to register link for container")
			}

			sem.Release(1)
			group.Done()
		}(c)
	}
	group.Wait()

	for c, notifyChan := range restartContainers {
		group.Add(1)
		go func(c *container.Container, chNotify chan struct{}) {
			_ = sem.Acquire(context.Background(), 1)

			logger := log.G(context.TODO()).WithField("container", c.ID)

			logger.Debug("starting container")

			// ignore errors here as this is a best effort to wait for children
			// (legacy links or container network) to be running before we try to start the container
			if children := daemon.GetDependentContainers(c); len(children) > 0 {
				timeout := time.NewTimer(5 * time.Second)
				defer timeout.Stop()

				for _, child := range children {
					if notifier, exists := restartContainers[child]; exists {
						select {
						case <-notifier:
						case <-timeout.C:
						}
					}
				}
			}

			if err := daemon.prepareMountPoints(c); err != nil {
				logger.WithError(err).Error("failed to prepare mount points for container")
			}
			if err := daemon.containerStart(context.Background(), cfg, c, "", "", true); err != nil {
				logger.WithError(err).Error("failed to start container")
			}
			close(chNotify)

			sem.Release(1)
			group.Done()
		}(c, notifyChan)
	}
	group.Wait()

	for id := range removeContainers {
		group.Add(1)
		go func(cid string) {
			_ = sem.Acquire(context.Background(), 1)

			if err := daemon.containerRm(&cfg.Config, cid, &backend.ContainerRmConfig{ForceRemove: true, RemoveVolume: true}); err != nil {
				log.G(context.TODO()).WithField("container", cid).WithError(err).Error("failed to remove container")
			}

			sem.Release(1)
			group.Done()
		}(id)
	}
	group.Wait()

	// any containers that were started above would already have had this done,
	// however we need to now prepare the mountpoints for the rest of the containers as well.
	// This shouldn't cause any issue running on the containers that already had this run.
	// This must be run after any containers with a restart policy so that containerized plugins
	// can have a chance to be running before we try to initialize them.
	for _, c := range containers {
		// if the container has restart policy, do not
		// prepare the mountpoints since it has been done on restarting.
		// This is to speed up the daemon start when a restart container
		// has a volume and the volume driver is not available.
		if _, ok := restartContainers[c]; ok {
			continue
		} else if _, ok := removeContainers[c.ID]; ok {
			// container is automatically removed, skip it.
			continue
		}

		group.Add(1)
		go func(c *container.Container) {
			_ = sem.Acquire(context.Background(), 1)

			if err := daemon.prepareMountPoints(c); err != nil {
				log.G(context.TODO()).WithField("container", c.ID).WithError(err).Error("failed to prepare mountpoints for container")
			}

			sem.Release(1)
			group.Done()
		}(c)
	}
	group.Wait()

	log.G(context.TODO()).Info("Loading containers: done.")

	return nil
}

// RestartSwarmContainers restarts any autostart container which has a
// swarm endpoint.
func (daemon *Daemon) RestartSwarmContainers() {
	daemon.restartSwarmContainers(context.Background(), daemon.config())
}

func (daemon *Daemon) restartSwarmContainers(ctx context.Context, cfg *configStore) {
	// parallelLimit is the maximum number of parallel startup jobs that we
	// allow (this is the limited used for all startup semaphores). The multiplier
	// (128) was chosen after some fairly significant benchmarking -- don't change
	// it unless you've tested it significantly (this value is adjusted if
	// RLIMIT_NOFILE is small to avoid EMFILE).
	parallelLimit := adjustParallelLimit(len(daemon.List()), 128*runtime.NumCPU())

	var group sync.WaitGroup
	sem := semaphore.NewWeighted(int64(parallelLimit))

	for _, c := range daemon.List() {
		if !c.IsRunning() && !c.IsPaused() {
			// Autostart all the containers which has a
			// swarm endpoint now that the cluster is
			// initialized.
			if cfg.AutoRestart && c.ShouldRestart() && c.NetworkSettings.HasSwarmEndpoint && c.HasBeenStartedBefore {
				group.Add(1)
				go func(c *container.Container) {
					if err := sem.Acquire(ctx, 1); err != nil {
						// ctx is done.
						group.Done()
						return
					}

					if err := daemon.containerStart(ctx, cfg, c, "", "", true); err != nil {
						log.G(ctx).WithField("container", c.ID).WithError(err).Error("failed to start swarm container")
					}

					sem.Release(1)
					group.Done()
				}(c)
			}
		}
	}
	group.Wait()
}

func (daemon *Daemon) registerLink(parent, child *container.Container, alias string) error {
	fullName := path.Join(parent.Name, alias)
	if err := daemon.containersReplica.ReserveName(fullName, child.ID); err != nil {
		if cerrdefs.IsConflict(err) {
			log.G(context.TODO()).Warnf("error registering link for %s, to %s, as alias %s, ignoring: %v", parent.ID, child.ID, alias, err)
			return nil
		}
		return err
	}
	daemon.linkIndex.link(parent, child, fullName)
	return nil
}

// DaemonJoinsCluster informs the daemon has joined the cluster and provides
// the handler to query the cluster component
func (daemon *Daemon) DaemonJoinsCluster(clusterProvider cluster.Provider) {
	daemon.setClusterProvider(clusterProvider)
}

// DaemonLeavesCluster informs the daemon has left the cluster
func (daemon *Daemon) DaemonLeavesCluster() {
	// Daemon is in charge of removing the attachable networks with
	// connected containers when the node leaves the swarm
	daemon.clearAttachableNetworks()
	// We no longer need the cluster provider, stop it now so that
	// the network agent will stop listening to cluster events.
	daemon.setClusterProvider(nil)
	// Wait for the networking cluster agent to stop
	daemon.netController.AgentStopWait()
	// Daemon is in charge of removing the ingress network when the
	// node leaves the swarm. Wait for job to be done or timeout.
	// This is called also on graceful daemon shutdown. We need to
	// wait, because the ingress release has to happen before the
	// network controller is stopped.

	if done, err := daemon.ReleaseIngress(); err == nil {
		timeout := time.NewTimer(5 * time.Second)
		defer timeout.Stop()

		select {
		case <-done:
		case <-timeout.C:
			log.G(context.TODO()).Warn("timeout while waiting for ingress network removal")
		}
	} else {
		log.G(context.TODO()).Warnf("failed to initiate ingress network removal: %v", err)
	}

	daemon.attachmentStore.ClearAttachments()
}

// setClusterProvider sets a component for querying the current cluster state.
func (daemon *Daemon) setClusterProvider(clusterProvider cluster.Provider) {
	daemon.clusterProvider = clusterProvider
	daemon.netController.SetClusterProvider(clusterProvider)
	daemon.attachableNetworkLock = locker.New()
}

// IsSwarmCompatible verifies if the current daemon
// configuration is compatible with the swarm mode
func (daemon *Daemon) IsSwarmCompatible() error {
	return daemon.config().IsSwarmCompatible()
}

// NewDaemon sets up everything for the daemon to be able to service
// requests from the webserver.
func NewDaemon(ctx context.Context, config *config.Config, pluginStore *plugin.Store, authzMiddleware *authorization.Middleware) (_ *Daemon, retErr error) {
	// Verify platform-specific requirements.
	// TODO(thaJeztah): this should be called before we try to create the daemon; perhaps together with the config validation.
	if err := checkSystem(); err != nil {
		return nil, err
	}

	registryService, err := registry.NewService(config.ServiceOptions)
	if err != nil {
		return nil, err
	}

	// Ensure that we have a correct root key limit for launching containers.
	if err := modifyRootKeyLimit(); err != nil {
		log.G(ctx).Warnf("unable to modify root key limit, number of containers could be limited by this quota: %v", err)
	}

	// Ensure we have compatible and valid configuration options
	if err := verifyDaemonSettings(config); err != nil {
		return nil, err
	}

	// Do we have a disabled network?
	config.DisableBridge = isBridgeNetworkDisabled(config)

	// Setup the resolv.conf
	setupResolvConf(config)

	idMapping, err := setupRemappedRoot(config)
	if err != nil {
		return nil, err
	}
	uid, gid := idMapping.RootPair()

	// set up the tmpDir to use a canonical path
	tmp, err := prepareTempDir(config.Root)
	if err != nil {
		return nil, fmt.Errorf("Unable to get the TempDir under %s: %s", config.Root, err)
	}
	realTmp, err := fileutils.ReadSymlinkedDirectory(tmp)
	if err != nil {
		return nil, fmt.Errorf("Unable to get the full path to the TempDir (%s): %s", tmp, err)
	}
	if isWindows {
		if err := os.MkdirAll(realTmp, 0); err != nil {
			return nil, fmt.Errorf("Unable to create the TempDir (%s): %s", realTmp, err)
		}
		_ = os.Setenv("TEMP", realTmp)
		_ = os.Setenv("TMP", realTmp)
	} else {
		_ = os.Setenv("TMPDIR", realTmp)
	}

	if err := initRuntimesDir(config); err != nil {
		return nil, err
	}
	rts, err := setupRuntimes(config)
	if err != nil {
		return nil, err
	}

	d := &Daemon{
		PluginStore: pluginStore,
		startupDone: make(chan struct{}),
	}
	cfgStore := &configStore{
		Config:   *config,
		Runtimes: rts,
	}
	d.configStore.Store(cfgStore)

	// TEST_INTEGRATION_USE_SNAPSHOTTER is used for integration tests only.
	if os.Getenv("TEST_INTEGRATION_USE_SNAPSHOTTER") != "" {
		d.usesSnapshotter = true
	} else {
		d.usesSnapshotter = config.Features["containerd-snapshotter"]
	}

	// Ensure the daemon is properly shutdown if there is a failure during
	// initialization
	defer func() {
		if retErr != nil {
			// Use a fresh context here. Passed context could be cancelled.
			if err := d.Shutdown(context.Background()); err != nil {
				log.G(ctx).Error(err)
			}
		}
	}()

	if err := d.setGenericResources(&cfgStore.Config); err != nil {
		return nil, err
	}
	// set up SIGUSR1 handler on Unix-like systems, or a Win32 global event
	// on Windows to dump Go routine stacks
	stackDumpDir := cfgStore.Root
	if execRoot := cfgStore.GetExecRoot(); execRoot != "" {
		stackDumpDir = execRoot
	}
	d.setupDumpStackTrap(stackDumpDir)

	if err := d.setupSeccompProfile(&cfgStore.Config); err != nil {
		return nil, err
	}

	// Set the default isolation mode (only applicable on Windows)
	if err := d.setDefaultIsolation(&cfgStore.Config); err != nil {
		return nil, fmt.Errorf("error setting default isolation mode: %v", err)
	}

	if err := configureMaxThreads(ctx); err != nil {
		log.G(ctx).Warnf("Failed to configure golang's threads limit: %v", err)
	}

	// ensureDefaultAppArmorProfile does nothing if apparmor is disabled
	if err := ensureDefaultAppArmorProfile(); err != nil {
		log.G(ctx).WithError(err).Error("Failed to ensure default apparmor profile is loaded")
	}

	daemonRepo := filepath.Join(cfgStore.Root, "containers")
	if err := user.MkdirAllAndChown(daemonRepo, 0o710, os.Getuid(), gid); err != nil {
		return nil, err
	}

	if isWindows {
		// Note that permissions (0o700) are ignored on Windows; passing them to
		// show intent only. We could consider using idtools.MkdirAndChown here
		// to apply an ACL.
		if err = os.Mkdir(filepath.Join(cfgStore.Root, "credentialspecs"), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}

	d.registryService = registryService
	dlogger.RegisterPluginGetter(d.PluginStore)

	if err := metrics.RegisterPlugin(d.PluginStore, filepath.Join(cfgStore.ExecRoot, "metrics.sock")); err != nil {
		return nil, err
	}

	const connTimeout = 60 * time.Second

	// Set the max backoff delay to match our containerd.WithTimeout(),
	// aligning with how containerd client's defaults sets this;
	// https://github.com/containerd/containerd/blob/v2.0.2/client/client.go#L129-L136
	backoffConfig := backoff.DefaultConfig
	backoffConfig.MaxDelay = connTimeout
	connParams := grpc.ConnectParams{
		Backoff: backoffConfig,
		// TODO: Remove after https://github.com/containerd/containerd/pull/11508
		MinConnectTimeout: connTimeout,
	}
	gopts := []grpc.DialOption{
		// ------------------------------------------------------------------
		// options below are copied from containerd client's default options
		//
		// We need to set these options, because setting any custom DialOptions
		// currently overwrites (not appends to) the defaults;
		// https://github.com/containerd/containerd/blob/v2.0.2/client/client.go#L129-L141
		//
		// TODO(thaJeztah): use containerd.WithExtraDialOpts() once https://github.com/containerd/containerd/pull/11276 is merged and in a release.
		// ------------------------------------------------------------------
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithConnectParams(connParams),
		grpc.WithContextDialer(dialer.ContextDialer),
		// ------------------------------------------------------------------
		// end of options copied from containerd client's default
		// ------------------------------------------------------------------
		grpc.WithStatsHandler(tracing.ClientStatsHandler(otelgrpc.WithTracerProvider(otel.GetTracerProvider()))),
		grpc.WithUnaryInterceptor(grpcerrors.UnaryClientInterceptor),
		grpc.WithStreamInterceptor(grpcerrors.StreamClientInterceptor),
	}

	if cfgStore.ContainerdAddr != "" {
		log.G(ctx).WithFields(log.Fields{
			"address": cfgStore.ContainerdAddr,
			"timeout": connTimeout,
		}).Info("Creating a containerd client")
		d.containerdClient, err = containerd.New(
			cfgStore.ContainerdAddr,
			containerd.WithDefaultNamespace(cfgStore.ContainerdNamespace),
			containerd.WithDialOpts(gopts),
			containerd.WithTimeout(connTimeout),
		)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to dial %q", cfgStore.ContainerdAddr)
		}
	}

	createPluginExec := func(m *plugin.Manager) (plugin.Executor, error) {
		var pluginCli *containerd.Client

		if cfgStore.ContainerdAddr != "" {
			pluginCli, err = containerd.New(
				cfgStore.ContainerdAddr,
				containerd.WithDefaultNamespace(cfgStore.ContainerdPluginNamespace),
				containerd.WithDialOpts(gopts),
				containerd.WithTimeout(connTimeout),
			)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to dial %q", cfgStore.ContainerdAddr)
			}
		}

		var (
			shim     string
			shimOpts any
		)
		if runtime.GOOS != "windows" {
			shim, shimOpts, err = rts.Get("")
			if err != nil {
				return nil, err
			}
		}
		return pluginexec.New(ctx, getPluginExecRoot(&cfgStore.Config), pluginCli, cfgStore.ContainerdPluginNamespace, m, shim, shimOpts)
	}

	// Plugin system initialization should happen before restore. Do not change order.
	d.pluginManager, err = plugin.NewManager(plugin.ManagerConfig{
		Root:               filepath.Join(cfgStore.Root, "plugins"),
		ExecRoot:           getPluginExecRoot(&cfgStore.Config),
		Store:              d.PluginStore,
		CreateExecutor:     createPluginExec,
		RegistryService:    registryService,
		LiveRestoreEnabled: cfgStore.LiveRestoreEnabled,
		LogPluginEvent:     d.LogPluginEvent, // todo: make private
		AuthzMiddleware:    authzMiddleware,
	})
	if err != nil {
		return nil, errors.Wrap(err, "couldn't create plugin manager")
	}

	d.defaultLogConfig, err = defaultLogConfig(&cfgStore.Config)
	if err != nil {
		return nil, errors.Wrap(err, "failed to set log opts")
	}
	log.G(ctx).Debugf("Using default logging driver %s", d.defaultLogConfig.Type)

	d.volumes, err = volumesservice.NewVolumeService(cfgStore.Root, d.PluginStore, idtools.Identity{UID: uid, GID: gid}, d)
	if err != nil {
		return nil, err
	}

	// Check if Devices cgroup is mounted, it is hard requirement for container security,
	// on Linux.
	//
	// Important: we call getSysInfo() directly here, without storing the results,
	// as networking has not yet been set up, so we only have partial system info
	// at this point.
	//
	// TODO(thaJeztah) add a utility to only collect the CgroupDevicesEnabled information
	if runtime.GOOS == "linux" && !userns.RunningInUserNS() && !getSysInfo(&cfgStore.Config).CgroupDevicesEnabled {
		return nil, errors.New("Devices cgroup isn't mounted")
	}

	d.id, err = LoadOrCreateID(cfgStore.Root)
	if err != nil {
		return nil, err
	}
	d.repository = daemonRepo
	d.containers = container.NewMemoryStore()
	if d.containersReplica, err = container.NewViewDB(); err != nil {
		return nil, err
	}
	d.execCommands = container.NewExecStore()
	d.statsCollector = d.newStatsCollector(1 * time.Second)

	d.EventsService = events.New()
	d.root = cfgStore.Root
	d.idMapping = idMapping

	d.linkIndex = newLinkIndex()

	// On Windows we don't support the environment variable, or a user supplied graphdriver
	// Unix platforms however run a single graphdriver for all containers, and it can
	// be set through an environment variable, a daemon start parameter, or chosen through
	// initialization of the layerstore through driver priority order for example.
	driverName := os.Getenv("DOCKER_DRIVER")
	if isWindows && d.UsesSnapshotter() {
		// Containerd WCOW snapshotter
		driverName = "windows"
	} else if isWindows {
		// Docker WCOW graphdriver
		driverName = "windowsfilter"
	} else if driverName != "" {
		log.G(ctx).Infof("Setting the storage driver from the $DOCKER_DRIVER environment variable (%s)", driverName)
	} else {
		driverName = cfgStore.GraphDriver
	}

	if d.UsesSnapshotter() {
		if os.Getenv("TEST_INTEGRATION_USE_SNAPSHOTTER") != "" {
			log.G(ctx).Warn("Enabling containerd snapshotter through the $TEST_INTEGRATION_USE_SNAPSHOTTER environment variable. This should only be used for testing.")
		}
		log.G(ctx).Info("Starting daemon with containerd snapshotter integration enabled")

		// FIXME(thaJeztah): implement automatic snapshotter-selection similar to graph-driver selection; see https://github.com/moby/moby/issues/44076
		if driverName == "" {
			driverName = defaults.DefaultSnapshotter
		}

		// Configure and validate the kernels security support. Note this is a Linux/FreeBSD
		// operation only, so it is safe to pass *just* the runtime OS graphdriver.
		if err := configureKernelSecuritySupport(&cfgStore.Config, driverName); err != nil {
			return nil, err
		}
		d.imageService = ctrd.NewService(ctrd.ImageServiceConfig{
			Client:          d.containerdClient,
			Containers:      d.containers,
			Snapshotter:     driverName,
			RegistryHosts:   d.RegistryHosts,
			Registry:        d.registryService,
			EventsService:   d.EventsService,
			IDMapping:       idMapping,
			RefCountMounter: snapshotter.NewMounter(config.Root, driverName, idMapping),
		})
	} else {
		layerStore, err := layer.NewStoreFromOptions(layer.StoreOptions{
			Root:               cfgStore.Root,
			GraphDriver:        driverName,
			GraphDriverOptions: cfgStore.GraphOptions,
			IDMapping:          idMapping,
		})
		if err != nil {
			return nil, err
		}

		// Configure and validate the kernels security support. Note this is a Linux/FreeBSD
		// operation only, so it is safe to pass *just* the runtime OS graphdriver.
		if err := configureKernelSecuritySupport(&cfgStore.Config, layerStore.DriverName()); err != nil {
			return nil, err
		}

		imageRoot := filepath.Join(cfgStore.Root, "image", layerStore.DriverName())
		ifs, err := image.NewFSStoreBackend(filepath.Join(imageRoot, "imagedb"))
		if err != nil {
			return nil, err
		}

		// We have a single tag/reference store for the daemon globally. However, it's
		// stored under the graphdriver. On host platforms which only support a single
		// container OS, but multiple selectable graphdrivers, this means depending on which
		// graphdriver is chosen, the global reference store is under there. For
		// platforms which support multiple container operating systems, this is slightly
		// more problematic as where does the global ref store get located? Fortunately,
		// for Windows, which is currently the only daemon supporting multiple container
		// operating systems, the list of graphdrivers available isn't user configurable.
		// For backwards compatibility, we just put it under the windowsfilter
		// directory regardless.
		refStoreLocation := filepath.Join(imageRoot, `repositories.json`)
		rs, err := refstore.NewReferenceStore(refStoreLocation)
		if err != nil {
			return nil, fmt.Errorf("Couldn't create reference store repository: %s", err)
		}
		d.ReferenceStore = rs

		imageStore, err := image.NewImageStore(ifs, layerStore)
		if err != nil {
			return nil, err
		}

		distributionMetadataStore, err := dmetadata.NewFSMetadataStore(filepath.Join(imageRoot, "distribution"))
		if err != nil {
			return nil, err
		}

		imgSvcConfig := images.ImageServiceConfig{
			ContainerStore:            d.containers,
			DistributionMetadataStore: distributionMetadataStore,
			EventsService:             d.EventsService,
			ImageStore:                imageStore,
			LayerStore:                layerStore,
			MaxConcurrentDownloads:    config.MaxConcurrentDownloads,
			MaxConcurrentUploads:      config.MaxConcurrentUploads,
			MaxDownloadAttempts:       config.MaxDownloadAttempts,
			ReferenceStore:            rs,
			RegistryService:           registryService,
			ContentNamespace:          config.ContainerdNamespace,
		}

		// containerd is not currently supported with Windows.
		// So sometimes d.containerdCli will be nil
		// In that case we'll create a local content store... but otherwise we'll use containerd
		if d.containerdClient != nil {
			imgSvcConfig.Leases = d.containerdClient.LeasesService()
			imgSvcConfig.ContentStore = d.containerdClient.ContentStore()
		} else {
			imgSvcConfig.ContentStore, imgSvcConfig.Leases, err = d.configureLocalContentStore(config.ContainerdNamespace)
			if err != nil {
				return nil, err
			}
		}

		// TODO: imageStore, distributionMetadataStore, and ReferenceStore are only
		// used above to run migration. They could be initialized in ImageService
		// if migration is called from daemon/images. layerStore might move as well.
		d.imageService = images.NewImageService(imgSvcConfig)

		log.G(ctx).Debugf("Max Concurrent Downloads: %d", imgSvcConfig.MaxConcurrentDownloads)
		log.G(ctx).Debugf("Max Concurrent Uploads: %d", imgSvcConfig.MaxConcurrentUploads)
		log.G(ctx).Debugf("Max Download Attempts: %d", imgSvcConfig.MaxDownloadAttempts)
	}

	go d.execCommandGC()

	if err := d.initLibcontainerd(ctx, &cfgStore.Config); err != nil {
		return nil, err
	}

	if err := d.restore(cfgStore); err != nil {
		return nil, err
	}
	close(d.startupDone)

	info, err := d.SystemInfo(ctx)
	if err != nil {
		return nil, err
	}
	for _, w := range info.Warnings {
		log.G(ctx).Warn(w)
	}

	metrics.EngineInfo.WithValues(
		dockerversion.Version,
		dockerversion.GitCommit,
		info.Architecture,
		info.Driver,
		info.KernelVersion,
		info.OperatingSystem,
		info.OSType,
		info.OSVersion,
		info.ID,
	).Set(1)
	metrics.EngineCPUs.Set(float64(info.NCPU))
	metrics.EngineMemory.Set(float64(info.MemTotal))

	log.G(ctx).WithFields(log.Fields{
		"version":                dockerversion.Version,
		"commit":                 dockerversion.GitCommit,
		"storage-driver":         d.ImageService().StorageDriver(),
		"containerd-snapshotter": d.UsesSnapshotter(),
	}).Info("Docker daemon")

	return d, nil
}

// DistributionServices returns services controlling daemon storage
func (daemon *Daemon) DistributionServices() images.DistributionServices {
	return daemon.imageService.DistributionServices()
}

func (daemon *Daemon) waitForStartupDone() {
	<-daemon.startupDone
}

func (daemon *Daemon) shutdownContainer(c *container.Container) error {
	ctx := context.WithoutCancel(context.TODO())

	// If container failed to exit in stopTimeout seconds of SIGTERM, then using the force
	if err := daemon.containerStop(ctx, c, containertypes.StopOptions{}); err != nil {
		return fmt.Errorf("Failed to stop container %s with error: %v", c.ID, err)
	}

	// Wait without timeout for the container to exit.
	// Ignore the result.
	<-c.Wait(ctx, containertypes.WaitConditionNotRunning)
	return nil
}

// ShutdownTimeout returns the timeout (in seconds) before containers are forcibly
// killed during shutdown. The default timeout can be configured both on the daemon
// and per container, and the longest timeout will be used. A grace-period of
// 5 seconds is added to the configured timeout.
//
// A negative (-1) timeout means "indefinitely", which means that containers
// are not forcibly killed, and the daemon shuts down after all containers exit.
func (daemon *Daemon) ShutdownTimeout() int {
	return daemon.shutdownTimeout(&daemon.config().Config)
}

func (daemon *Daemon) shutdownTimeout(cfg *config.Config) int {
	shutdownTimeout := cfg.ShutdownTimeout
	if shutdownTimeout < 0 {
		return -1
	}
	if daemon.containers == nil {
		return shutdownTimeout
	}

	graceTimeout := 5
	for _, c := range daemon.containers.List() {
		stopTimeout := c.StopTimeout()
		if stopTimeout < 0 {
			return -1
		}
		if stopTimeout+graceTimeout > shutdownTimeout {
			shutdownTimeout = stopTimeout + graceTimeout
		}
	}
	return shutdownTimeout
}

// Shutdown stops the daemon.
func (daemon *Daemon) Shutdown(ctx context.Context) error {
	daemon.shutdown = true
	// Keep mounts and networking running on daemon shutdown if
	// we are to keep containers running and restore them.

	cfg := &daemon.config().Config
	if cfg.LiveRestoreEnabled && daemon.containers != nil {
		// check if there are any running containers, if none we should do some cleanup
		if ls, err := daemon.Containers(ctx, &containertypes.ListOptions{}); len(ls) != 0 || err != nil {
			// metrics plugins still need some cleanup
			metrics.CleanupPlugin(daemon.PluginStore)
			return err
		}
	}

	if daemon.containers != nil {
		log.G(ctx).Debugf("daemon configured with a %d seconds minimum shutdown timeout", cfg.ShutdownTimeout)
		log.G(ctx).Debugf("start clean shutdown of all containers with a %d seconds timeout...", daemon.shutdownTimeout(cfg))
		daemon.containers.ApplyAll(func(c *container.Container) {
			if !c.IsRunning() {
				return
			}
			logger := log.G(ctx).WithField("container", c.ID)
			logger.Debug("shutting down container")
			if err := daemon.shutdownContainer(c); err != nil {
				logger.WithError(err).Error("failed to shut down container")
				return
			}
			if mountid, err := daemon.imageService.GetLayerMountID(c.ID); err == nil {
				daemon.cleanupMountsByID(mountid)
			}
			logger.Debugf("shut down container")
		})
	}

	if daemon.volumes != nil {
		if err := daemon.volumes.Shutdown(); err != nil {
			log.G(ctx).Errorf("Error shutting down volume store: %v", err)
		}
	}

	if daemon.imageService != nil {
		if err := daemon.imageService.Cleanup(); err != nil {
			log.G(ctx).Error(err)
		}
	}

	// If we are part of a cluster, clean up cluster's stuff
	if daemon.clusterProvider != nil {
		log.G(ctx).Debugf("start clean shutdown of cluster resources...")
		daemon.DaemonLeavesCluster()
	}

	metrics.CleanupPlugin(daemon.PluginStore)

	// Shutdown plugins after containers and layerstore. Don't change the order.
	daemon.pluginShutdown()

	// trigger libnetwork Stop only if it's initialized
	if daemon.netController != nil {
		daemon.netController.Stop()
	}

	if daemon.containerdClient != nil {
		daemon.containerdClient.Close()
	}

	if daemon.mdDB != nil {
		daemon.mdDB.Close()
	}

	return daemon.cleanupMounts(cfg)
}

// Mount sets container.BaseFS
func (daemon *Daemon) Mount(container *container.Container) error {
	ctx := context.TODO()

	if container.RWLayer == nil {
		return errors.New("RWLayer of container " + container.ID + " is unexpectedly nil")
	}
	dir, err := container.RWLayer.Mount(container.GetMountLabel())
	if err != nil {
		return err
	}
	log.G(ctx).WithFields(log.Fields{"container": container.ID, "root": dir, "storage-driver": container.Driver}).Debug("container mounted via layerStore")

	if container.BaseFS != "" && container.BaseFS != dir {
		// The mount path reported by the graph driver should always be trusted on Windows, since the
		// volume path for a given mounted layer may change over time.  This should only be an error
		// on non-Windows operating systems.
		if runtime.GOOS != "windows" {
			daemon.Unmount(container)
			driver := daemon.ImageService().StorageDriver()
			return fmt.Errorf("driver %s is returning inconsistent paths for container %s ('%s' then '%s')",
				driver, container.ID, container.BaseFS, dir)
		}
	}
	container.BaseFS = dir // TODO: combine these fields
	return nil
}

// Unmount unsets the container base filesystem
func (daemon *Daemon) Unmount(container *container.Container) error {
	ctx := context.TODO()
	if container.RWLayer == nil {
		return errors.New("RWLayer of container " + container.ID + " is unexpectedly nil")
	}
	if err := container.RWLayer.Unmount(); err != nil {
		log.G(ctx).WithField("container", container.ID).WithError(err).Error("error unmounting container")
		return err
	}

	return nil
}

// Subnets return the IPv4 and IPv6 subnets of networks that are manager by Docker.
func (daemon *Daemon) Subnets() ([]net.IPNet, []net.IPNet) {
	var v4Subnets []net.IPNet
	var v6Subnets []net.IPNet

	for _, managedNetwork := range daemon.netController.Networks(context.TODO()) {
		v4infos, v6infos := managedNetwork.IpamInfo()
		for _, info := range v4infos {
			if info.IPAMData.Pool != nil {
				v4Subnets = append(v4Subnets, *info.IPAMData.Pool)
			}
		}
		for _, info := range v6infos {
			if info.IPAMData.Pool != nil {
				v6Subnets = append(v6Subnets, *info.IPAMData.Pool)
			}
		}
	}

	return v4Subnets, v6Subnets
}

// prepareTempDir prepares and returns the default directory to use
// for temporary files.
// If it doesn't exist, it is created. If it exists, its content is removed.
func prepareTempDir(rootDir string) (string, error) {
	var tmpDir string
	if tmpDir = os.Getenv("DOCKER_TMPDIR"); tmpDir == "" {
		tmpDir = filepath.Join(rootDir, "tmp")
		newName := tmpDir + "-old"
		if err := os.Rename(tmpDir, newName); err == nil {
			go func() {
				if err := os.RemoveAll(newName); err != nil {
					log.G(context.TODO()).Warnf("failed to delete old tmp directory: %s", newName)
				}
			}()
		} else if !os.IsNotExist(err) {
			log.G(context.TODO()).Warnf("failed to rename %s for background deletion: %s. Deleting synchronously", tmpDir, err)
			if err := os.RemoveAll(tmpDir); err != nil {
				log.G(context.TODO()).Warnf("failed to delete old tmp directory: %s", tmpDir)
			}
		}
	}
	return tmpDir, user.MkdirAllAndChown(tmpDir, 0o700, os.Getuid(), os.Getegid())
}

func (daemon *Daemon) setGenericResources(conf *config.Config) error {
	genericResources, err := config.ParseGenericResources(conf.NodeGenericResources)
	if err != nil {
		return err
	}

	daemon.genericResources = genericResources

	return nil
}

// IsShuttingDown tells whether the daemon is shutting down or not
func (daemon *Daemon) IsShuttingDown() bool {
	return daemon.shutdown
}

func isBridgeNetworkDisabled(conf *config.Config) bool {
	return conf.BridgeConfig.Iface == config.DisableNetworkBridge
}

func (daemon *Daemon) networkOptions(conf *config.Config, pg plugingetter.PluginGetter, hostID string, activeSandboxes map[string]any) ([]nwconfig.Option, error) {
	options := []nwconfig.Option{
		nwconfig.OptionDataDir(filepath.Join(conf.Root, config.LibnetDataPath)),
		nwconfig.OptionExecRoot(conf.GetExecRoot()),
		nwconfig.OptionDefaultDriver(network.DefaultNetwork),
		nwconfig.OptionDefaultNetwork(network.DefaultNetwork),
		nwconfig.OptionLabels(conf.Labels),
		nwconfig.OptionNetworkControlPlaneMTU(conf.NetworkControlPlaneMTU),
		nwconfig.OptionFirewallBackend(conf.FirewallBackend),
		driverOptions(conf),
	}

	defaultAddressPools := ipamutils.GetLocalScopeDefaultNetworks()
	if len(conf.NetworkConfig.DefaultAddressPools.Value()) > 0 {
		defaultAddressPools = conf.NetworkConfig.DefaultAddressPools.Value()
	}
	// If the Engine admin don't configure default-address-pools or if they
	// don't provide any IPv6 prefix, we derive a ULA prefix from the daemon's
	// hostID and add it to the pools. This makes dynamic IPv6 subnet
	// allocation possible out-of-the-box.
	if !slices.ContainsFunc(defaultAddressPools, func(nw *ipamutils.NetworkToSplit) bool {
		return nw.Base.Addr().Is6() && !nw.Base.Addr().Is4In6()
	}) {
		defaultAddressPools = append(defaultAddressPools, deriveULABaseNetwork(hostID))
	}
	options = append(options, nwconfig.OptionDefaultAddressPoolConfig(defaultAddressPools))

	if conf.LiveRestoreEnabled && len(activeSandboxes) != 0 {
		options = append(options, nwconfig.OptionActiveSandboxes(activeSandboxes))
	}
	if pg != nil {
		options = append(options, nwconfig.OptionPluginGetter(pg))
	}

	return options, nil
}

// deriveULABaseNetwork derives a Global ID from the provided hostID and
// appends it to the ULA prefix (with L bit set) to generate a ULA prefix
// unique to this host. The returned ipamutils.NetworkToSplit is stable over
// time if hostID doesn't change.
//
// This is loosely based on the algorithm described in https://datatracker.ietf.org/doc/html/rfc4193#section-3.2.2.
func deriveULABaseNetwork(hostID string) *ipamutils.NetworkToSplit {
	sha := sha256.Sum256([]byte(hostID))
	gid := binary.BigEndian.Uint64(sha[:]) & (1<<40 - 1) // Keep the 40 least significant bits.
	addr := ipbits.Add(netip.MustParseAddr("fd00::"), gid, 80)

	return &ipamutils.NetworkToSplit{
		Base: netip.PrefixFrom(addr, 48),
		Size: 64,
	}
}

// GetCluster returns the cluster
func (daemon *Daemon) GetCluster() Cluster {
	return daemon.cluster
}

// SetCluster sets the cluster
func (daemon *Daemon) SetCluster(cluster Cluster) {
	daemon.cluster = cluster
}

func (daemon *Daemon) pluginShutdown() {
	manager := daemon.pluginManager
	// Check for a valid manager object. In error conditions, daemon init can fail
	// and shutdown called, before plugin manager is initialized.
	if manager != nil {
		manager.Shutdown()
	}
}

// PluginManager returns current pluginManager associated with the daemon
func (daemon *Daemon) PluginManager() *plugin.Manager { // set up before daemon to avoid this method
	return daemon.pluginManager
}

// PluginGetter returns current pluginStore associated with the daemon
func (daemon *Daemon) PluginGetter() *plugin.Store {
	return daemon.PluginStore
}

// CreateDaemonRoot creates the root for the daemon
func CreateDaemonRoot(config *config.Config) error {
	// get the canonical path to the Docker root directory
	var realRoot string
	if _, err := os.Stat(config.Root); err != nil && os.IsNotExist(err) {
		realRoot = config.Root
	} else {
		realRoot, err = fileutils.ReadSymlinkedDirectory(config.Root)
		if err != nil {
			return fmt.Errorf("Unable to get the full path to root (%s): %s", config.Root, err)
		}
	}

	idMapping, err := setupRemappedRoot(config)
	if err != nil {
		return err
	}
	uid, gid := idMapping.RootPair()
	return setupDaemonRoot(config, realRoot, uid, gid)
}

// RemapContainerdNamespaces returns the right containerd namespaces to use:
// - if they are not already set in the config file
// -  and the daemon is running with user namespace remapping enabled
// Then it will return new namespace names, otherwise it will return the existing
// namespaces
func RemapContainerdNamespaces(config *config.Config) (ns string, pluginNs string, err error) {
	idMapping, err := setupRemappedRoot(config)
	if err != nil {
		return "", "", err
	}
	if idMapping.Empty() {
		return config.ContainerdNamespace, config.ContainerdPluginNamespace, nil
	}
	uid, gid := idMapping.RootPair()

	ns = config.ContainerdNamespace
	if _, ok := config.ValuesSet["containerd-namespace"]; !ok {
		ns = fmt.Sprintf("%s-%d.%d", config.ContainerdNamespace, uid, gid)
	}

	pluginNs = config.ContainerdPluginNamespace
	if _, ok := config.ValuesSet["containerd-plugin-namespace"]; !ok {
		pluginNs = fmt.Sprintf("%s-%d.%d", config.ContainerdPluginNamespace, uid, gid)
	}

	return ns, pluginNs, nil
}

// checkpointAndSave grabs a container lock to safely call container.CheckpointTo
func (daemon *Daemon) checkpointAndSave(container *container.Container) error {
	container.Lock()
	defer container.Unlock()
	if err := container.CheckpointTo(context.TODO(), daemon.containersReplica); err != nil {
		return fmt.Errorf("Error saving container state: %v", err)
	}
	return nil
}

// because the CLI sends a -1 when it wants to unset the swappiness value
// we need to clear it on the server side
func fixMemorySwappiness(resources *containertypes.Resources) {
	if resources.MemorySwappiness != nil && *resources.MemorySwappiness == -1 {
		resources.MemorySwappiness = nil
	}
}

// GetAttachmentStore returns current attachment store associated with the daemon
func (daemon *Daemon) GetAttachmentStore() *network.AttachmentStore {
	return &daemon.attachmentStore
}

// IdentityMapping returns uid/gid mapping or a SID (in the case of Windows) for the builder
func (daemon *Daemon) IdentityMapping() user.IdentityMapping {
	return daemon.idMapping
}

// ImageService returns the Daemon's ImageService
func (daemon *Daemon) ImageService() ImageService {
	return daemon.imageService
}

// ImageBackend returns an image-backend for Swarm and the distribution router.
func (daemon *Daemon) ImageBackend() executorpkg.ImageBackend {
	return &imageBackend{
		ImageService:    daemon.imageService,
		registryService: daemon.registryService,
	}
}

// RegistryService returns the Daemon's RegistryService
func (daemon *Daemon) RegistryService() *registry.Service {
	return daemon.registryService
}

// BuilderBackend returns the backend used by builder
func (daemon *Daemon) BuilderBackend() builder.Backend {
	return struct {
		*Daemon
		ImageService
	}{daemon, daemon.imageService}
}

// RawSysInfo returns *sysinfo.SysInfo .
func (daemon *Daemon) RawSysInfo() *sysinfo.SysInfo {
	daemon.sysInfoOnce.Do(func() {
		// We check if sysInfo is not set here, to allow some test to
		// override the actual sysInfo.
		if daemon.sysInfo == nil {
			daemon.sysInfo = getSysInfo(&daemon.config().Config)
		}
	})

	return daemon.sysInfo
}

// imageBackend is used to satisfy the [executorpkg.ImageBackend] and
// [github.com/docker/docker/api/server/router/distribution.Backend]
// interfaces.
type imageBackend struct {
	ImageService
	registryService *registry.Service
}

// GetRepositories returns a list of repositories configured for the given
// reference. Multiple repositories can be returned if the reference is for
// the default (Docker Hub) registry and a mirror is configured, but it omits
// registries that were not reachable (pinging the /v2/ endpoint failed).
//
// It returns an error if it was unable to reach any of the registries for
// the given reference, or if the provided reference is invalid.
func (i *imageBackend) GetRepositories(ctx context.Context, ref reference.Named, authConfig *registrytypes.AuthConfig) ([]dist.Repository, error) {
	return distribution.GetRepositories(ctx, ref, &distribution.ImagePullConfig{
		Config: distribution.Config{
			AuthConfig:      authConfig,
			RegistryService: i.registryService,
		},
	})
}

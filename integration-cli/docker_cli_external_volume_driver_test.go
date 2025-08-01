package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	volumetypes "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/integration-cli/cli"
	"github.com/docker/docker/integration-cli/daemon"
	"github.com/docker/docker/pkg/plugins"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/docker/testutil"
	testdaemon "github.com/docker/docker/testutil/daemon"
	"github.com/docker/docker/volume"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

const volumePluginName = "test-external-volume-driver"

type eventCounter struct {
	activations int
	creations   int
	removals    int
	mounts      int
	unmounts    int
	paths       int
	lists       int
	gets        int
	caps        int
}

type DockerExternalVolumeSuite struct {
	ds *DockerSuite
	d  *daemon.Daemon
	*volumePlugin
}

func (s *DockerExternalVolumeSuite) SetUpTest(ctx context.Context, t *testing.T) {
	testRequires(t, testEnv.IsLocalDaemon)
	s.d = daemon.New(t, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
	s.ec = &eventCounter{}
}

func (s *DockerExternalVolumeSuite) TearDownTest(ctx context.Context, t *testing.T) {
	if s.d != nil {
		s.d.Stop(t)
		s.ds.TearDownTest(ctx, t)
	}
}

func (s *DockerExternalVolumeSuite) SetUpSuite(ctx context.Context, t *testing.T) {
	s.volumePlugin = newVolumePlugin(t, volumePluginName)
}

type volumePlugin struct {
	ec *eventCounter
	*httptest.Server
	vols map[string]vol
}

type vol struct {
	Name       string
	Mountpoint string
	Ninja      bool // hack used to trigger a null volume return on `Get`
	Status     map[string]interface{}
	Options    map[string]string
}

func (p *volumePlugin) Close() {
	p.Server.Close()
}

func newVolumePlugin(t *testing.T, name string) *volumePlugin {
	mux := http.NewServeMux()
	s := &volumePlugin{Server: httptest.NewServer(mux), ec: &eventCounter{}, vols: make(map[string]vol)}

	type pluginRequest struct {
		Name string
		Opts map[string]string
		ID   string
	}

	type pluginResp struct {
		Mountpoint string `json:",omitempty"`
		Err        string `json:",omitempty"`
	}

	read := func(b io.ReadCloser) (pluginRequest, error) {
		defer b.Close()
		var pr pluginRequest
		err := json.NewDecoder(b).Decode(&pr)
		return pr, err
	}

	send := func(w http.ResponseWriter, data interface{}) {
		switch d := data.(type) {
		case error:
			http.Error(w, d.Error(), http.StatusInternalServerError)
		case string:
			w.Header().Set("Content-Type", plugins.VersionMimetype)
			_, _ = fmt.Fprintln(w, d)
		default:
			w.Header().Set("Content-Type", plugins.VersionMimetype)
			err := json.NewEncoder(w).Encode(&data)
			if err != nil {
				t.Logf("Error encoding plugin response: %v", err)
			}
		}
	}

	mux.HandleFunc("/Plugin.Activate", func(w http.ResponseWriter, r *http.Request) {
		s.ec.activations++
		send(w, `{"Implements": ["VolumeDriver"]}`)
	})

	mux.HandleFunc("/VolumeDriver.Create", func(w http.ResponseWriter, r *http.Request) {
		s.ec.creations++
		pr, err := read(r.Body)
		if err != nil {
			send(w, err)
			return
		}
		_, isNinja := pr.Opts["ninja"]
		status := map[string]interface{}{"Hello": "world"}
		s.vols[pr.Name] = vol{Name: pr.Name, Ninja: isNinja, Status: status, Options: pr.Opts}
		send(w, nil)
	})

	mux.HandleFunc("/VolumeDriver.List", func(w http.ResponseWriter, r *http.Request) {
		s.ec.lists++
		vols := make([]vol, 0, len(s.vols))
		for _, v := range s.vols {
			if v.Ninja {
				continue
			}
			vols = append(vols, v)
		}
		send(w, map[string][]vol{"Volumes": vols})
	})

	mux.HandleFunc("/VolumeDriver.Get", func(w http.ResponseWriter, r *http.Request) {
		s.ec.gets++
		pr, err := read(r.Body)
		if err != nil {
			send(w, err)
			return
		}

		v, exists := s.vols[pr.Name]
		if !exists {
			send(w, `{"Err": "no such volume"}`)
		}

		if v.Ninja {
			send(w, map[string]vol{})
			return
		}

		v.Mountpoint = hostVolumePath(pr.Name)
		send(w, map[string]vol{"Volume": v})
	})

	mux.HandleFunc("/VolumeDriver.Remove", func(w http.ResponseWriter, r *http.Request) {
		s.ec.removals++
		pr, err := read(r.Body)
		if err != nil {
			send(w, err)
			return
		}

		v, ok := s.vols[pr.Name]
		if !ok {
			send(w, nil)
			return
		}

		if err := os.RemoveAll(hostVolumePath(v.Name)); err != nil {
			send(w, &pluginResp{Err: err.Error()})
			return
		}
		delete(s.vols, v.Name)
		send(w, nil)
	})

	mux.HandleFunc("/VolumeDriver.Path", func(w http.ResponseWriter, r *http.Request) {
		s.ec.paths++

		pr, err := read(r.Body)
		if err != nil {
			send(w, err)
			return
		}
		p := hostVolumePath(pr.Name)
		send(w, &pluginResp{Mountpoint: p})
	})

	mux.HandleFunc("/VolumeDriver.Mount", func(w http.ResponseWriter, r *http.Request) {
		s.ec.mounts++

		pr, err := read(r.Body)
		if err != nil {
			send(w, err)
			return
		}

		if v, exists := s.vols[pr.Name]; exists {
			// Use this to simulate a mount failure
			if _, exists := v.Options["invalidOption"]; exists {
				send(w, errors.New("invalid argument"))
				return
			}
		}

		p := hostVolumePath(pr.Name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			send(w, &pluginResp{Err: err.Error()})
			return
		}

		if err := os.WriteFile(filepath.Join(p, "test"), []byte(s.Server.URL), 0o644); err != nil {
			send(w, err)
			return
		}

		if err := os.WriteFile(filepath.Join(p, "mountID"), []byte(pr.ID), 0o644); err != nil {
			send(w, err)
			return
		}

		send(w, &pluginResp{Mountpoint: p})
	})

	mux.HandleFunc("/VolumeDriver.Unmount", func(w http.ResponseWriter, r *http.Request) {
		s.ec.unmounts++

		_, err := read(r.Body)
		if err != nil {
			send(w, err)
			return
		}

		send(w, nil)
	})

	mux.HandleFunc("/VolumeDriver.Capabilities", func(w http.ResponseWriter, r *http.Request) {
		s.ec.caps++

		_, err := read(r.Body)
		if err != nil {
			send(w, err)
			return
		}

		send(w, `{"Capabilities": { "Scope": "global" }}`)
	})

	err := os.MkdirAll("/etc/docker/plugins", 0o755)
	assert.NilError(t, err)

	err = os.WriteFile("/etc/docker/plugins/"+name+".spec", []byte(s.Server.URL), 0o644)
	assert.NilError(t, err)
	return s
}

func (s *DockerExternalVolumeSuite) TearDownSuite(ctx context.Context, t *testing.T) {
	s.volumePlugin.Close()

	err := os.RemoveAll("/etc/docker/plugins")
	assert.NilError(t, err)
}

func (s *DockerExternalVolumeSuite) TestVolumeCLICreateOptionConflict(c *testing.T) {
	cli.DockerCmd(c, "volume", "create", "test")

	out, _, err := dockerCmdWithError("volume", "create", "test", "--driver", volumePluginName)
	assert.Assert(c, err != nil, "volume create exception name already in use with another driver")
	assert.Assert(c, is.Contains(out, "must be unique"))
	driver := cli.DockerCmd(c, "volume", "inspect", "--format={{ .Driver }}", "test").Stdout()
	_, _, err = dockerCmdWithError("volume", "create", "test", "--driver", strings.TrimSpace(driver))
	assert.NilError(c, err)
}

func (s *DockerExternalVolumeSuite) TestExternalVolumeDriverNamed(c *testing.T) {
	ctx := testutil.GetContext(c)
	s.d.StartWithBusybox(ctx, c)

	out, err := s.d.Cmd("run", "--rm", "--name", "test-data", "-v", "external-volume-test:/tmp/external-volume-test", "--volume-driver", volumePluginName, "busybox:latest", "cat", "/tmp/external-volume-test/test")
	assert.NilError(c, err, out)
	assert.Assert(c, is.Contains(out, s.Server.URL))
	_, err = s.d.Cmd("volume", "rm", "external-volume-test")
	assert.NilError(c, err)

	p := hostVolumePath("external-volume-test")
	_, err = os.Lstat(p)
	assert.ErrorContains(c, err, "")
	assert.Assert(c, os.IsNotExist(err), "Expected volume path in host to not exist: %s, %v\n", p, err)

	assert.Equal(c, s.ec.activations, 1)
	assert.Equal(c, s.ec.creations, 1)
	assert.Equal(c, s.ec.removals, 1)
	assert.Equal(c, s.ec.mounts, 1)
	assert.Equal(c, s.ec.unmounts, 1)
}

func (s *DockerExternalVolumeSuite) TestExternalVolumeDriverUnnamed(c *testing.T) {
	ctx := testutil.GetContext(c)
	s.d.StartWithBusybox(ctx, c)

	out, err := s.d.Cmd("run", "--rm", "--name", "test-data", "-v", "/tmp/external-volume-test", "--volume-driver", volumePluginName, "busybox:latest", "cat", "/tmp/external-volume-test/test")
	assert.NilError(c, err, out)
	assert.Assert(c, is.Contains(out, s.Server.URL))
	assert.Equal(c, s.ec.activations, 1)
	assert.Equal(c, s.ec.creations, 1)
	assert.Equal(c, s.ec.removals, 1)
	assert.Equal(c, s.ec.mounts, 1)
	assert.Equal(c, s.ec.unmounts, 1)
}

func (s *DockerExternalVolumeSuite) TestExternalVolumeDriverVolumesFrom(c *testing.T) {
	ctx := testutil.GetContext(c)
	s.d.StartWithBusybox(ctx, c)

	out, err := s.d.Cmd("run", "--name", "vol-test1", "-v", "/foo", "--volume-driver", volumePluginName, "busybox:latest")
	assert.NilError(c, err, out)

	out, err = s.d.Cmd("run", "--rm", "--volumes-from", "vol-test1", "--name", "vol-test2", "busybox", "ls", "/tmp")
	assert.NilError(c, err, out)

	out, err = s.d.Cmd("rm", "-fv", "vol-test1")
	assert.NilError(c, err, out)

	assert.Equal(c, s.ec.activations, 1)
	assert.Equal(c, s.ec.creations, 1)
	assert.Equal(c, s.ec.removals, 1)
	assert.Equal(c, s.ec.mounts, 2)
	assert.Equal(c, s.ec.unmounts, 2)
}

func (s *DockerExternalVolumeSuite) TestExternalVolumeDriverDeleteContainer(c *testing.T) {
	ctx := testutil.GetContext(c)
	s.d.StartWithBusybox(ctx, c)

	out, err := s.d.Cmd("run", "--name", "vol-test1", "-v", "/foo", "--volume-driver", volumePluginName, "busybox:latest")
	assert.NilError(c, err, out)

	out, err = s.d.Cmd("rm", "-fv", "vol-test1")
	assert.NilError(c, err, out)

	assert.Equal(c, s.ec.activations, 1)
	assert.Equal(c, s.ec.creations, 1)
	assert.Equal(c, s.ec.removals, 1)
	assert.Equal(c, s.ec.mounts, 1)
	assert.Equal(c, s.ec.unmounts, 1)
}

func hostVolumePath(name string) string {
	return fmt.Sprintf("/var/lib/docker/volumes/%s", name)
}

// Make sure a request to use a down driver doesn't block other requests
func (s *DockerExternalVolumeSuite) TestExternalVolumeDriverLookupNotBlocked(c *testing.T) {
	specPath := "/etc/docker/plugins/down-driver.spec"
	err := os.WriteFile(specPath, []byte("tcp://127.0.0.7:9999"), 0o644)
	assert.NilError(c, err)
	defer os.RemoveAll(specPath)

	chCmd1 := make(chan struct{})
	chCmd2 := make(chan error, 1)
	cmd1 := exec.Command(dockerBinary, "volume", "create", "-d", "down-driver")
	cmd2 := exec.Command(dockerBinary, "volume", "create")

	assert.NilError(c, cmd1.Start())
	defer cmd1.Process.Kill()
	time.Sleep(100 * time.Millisecond) // ensure API has been called
	assert.NilError(c, cmd2.Start())

	go func() {
		cmd1.Wait()
		close(chCmd1)
	}()
	go func() {
		chCmd2 <- cmd2.Wait()
	}()

	select {
	case <-chCmd1:
		cmd2.Process.Kill()
		c.Fatalf("volume create with down driver finished unexpectedly")
	case err := <-chCmd2:
		assert.NilError(c, err)
	case <-time.After(5 * time.Second):
		cmd2.Process.Kill()
		c.Fatal("volume creates are blocked by previous create requests when previous driver is down")
	}
}

func (s *DockerExternalVolumeSuite) TestExternalVolumeDriverRetryNotImmediatelyExists(c *testing.T) {
	ctx := testutil.GetContext(c)
	s.d.StartWithBusybox(ctx, c)
	driverName := "test-external-volume-driver-retry"

	errchan := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		if out, err := s.d.Cmd("run", "--rm", "--name", "test-data-retry", "-v", "external-volume-test:/tmp/external-volume-test", "--volume-driver", driverName, "busybox:latest"); err != nil {
			errchan <- fmt.Errorf("%v:\n%s", err, out)
		}
		close(errchan)
	}()

	<-started
	// wait for a retry to occur, then create spec to allow plugin to register
	time.Sleep(2 * time.Second)
	p := newVolumePlugin(c, driverName)
	defer p.Close()

	select {
	case err := <-errchan:
		assert.NilError(c, err)
	case <-time.After(8 * time.Second):
		c.Fatal("volume creates fail when plugin not immediately available")
	}

	_, err := s.d.Cmd("volume", "rm", "external-volume-test")
	assert.NilError(c, err)

	assert.Equal(c, p.ec.activations, 1)
	assert.Equal(c, p.ec.creations, 1)
	assert.Equal(c, p.ec.removals, 1)
	assert.Equal(c, p.ec.mounts, 1)
	assert.Equal(c, p.ec.unmounts, 1)
}

func (s *DockerExternalVolumeSuite) TestExternalVolumeDriverBindExternalVolume(c *testing.T) {
	cli.DockerCmd(c, "volume", "create", "-d", volumePluginName, "foo")
	cli.DockerCmd(c, "run", "-d", "--name", "testing", "-v", "foo:/bar", "busybox", "top")

	var mounts []struct {
		Name   string
		Driver string
	}
	out := inspectFieldJSON(c, "testing", "Mounts")
	assert.NilError(c, json.NewDecoder(strings.NewReader(out)).Decode(&mounts))
	assert.Equal(c, len(mounts), 1, out)
	assert.Equal(c, mounts[0].Name, "foo")
	assert.Equal(c, mounts[0].Driver, volumePluginName)
}

func (s *DockerExternalVolumeSuite) TestExternalVolumeDriverList(c *testing.T) {
	cli.DockerCmd(c, "volume", "create", "-d", volumePluginName, "abc3")
	out := cli.DockerCmd(c, "volume", "ls").Stdout()
	ls := strings.Split(strings.TrimSpace(out), "\n")
	assert.Equal(c, len(ls), 2, fmt.Sprintf("\n%s", out))

	vol := strings.Fields(ls[len(ls)-1])
	assert.Equal(c, len(vol), 2, fmt.Sprintf("%v", vol))
	assert.Equal(c, vol[0], volumePluginName)
	assert.Equal(c, vol[1], "abc3")

	assert.Equal(c, s.ec.lists, 1)
}

func (s *DockerExternalVolumeSuite) TestExternalVolumeDriverGet(c *testing.T) {
	out, _, err := dockerCmdWithError("volume", "inspect", "dummy")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, is.Contains(out, "No such volume"))
	assert.Equal(c, s.ec.gets, 1)

	cli.DockerCmd(c, "volume", "create", "test", "-d", volumePluginName)
	out = cli.DockerCmd(c, "volume", "inspect", "test").Stdout()

	type vol struct {
		Status map[string]string
	}
	var st []vol

	assert.NilError(c, json.Unmarshal([]byte(out), &st))
	assert.Equal(c, len(st), 1)
	assert.Equal(c, len(st[0].Status), 1, fmt.Sprintf("%v", st[0]))
	assert.Equal(c, st[0].Status["Hello"], "world", fmt.Sprintf("%v", st[0].Status))
}

func (s *DockerExternalVolumeSuite) TestExternalVolumeDriverWithDaemonRestart(c *testing.T) {
	cli.DockerCmd(c, "volume", "create", "-d", volumePluginName, "abc1")
	s.d.Restart(c)

	cli.DockerCmd(c, "run", "--name=test", "-v", "abc1:/foo", "busybox", "true")
	var mounts []container.MountPoint
	inspectFieldAndUnmarshall(c, "test", "Mounts", &mounts)
	assert.Equal(c, len(mounts), 1)
	assert.Equal(c, mounts[0].Driver, volumePluginName)
}

// Ensures that the daemon handles when the plugin responds to a `Get` request with a null volume and a null error.
// Prior the daemon would panic in this scenario.
func (s *DockerExternalVolumeSuite) TestExternalVolumeDriverGetEmptyResponse(c *testing.T) {
	s.d.Start(c)

	out, err := s.d.Cmd("volume", "create", "-d", volumePluginName, "abc2", "--opt", "ninja=1")
	assert.NilError(c, err, out)

	out, err = s.d.Cmd("volume", "inspect", "abc2")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, is.Contains(out, "No such volume"))
}

// Ensure only cached paths are used in volume list to prevent N+1 calls to `VolumeDriver.Path`
//
// TODO(@cpuguy83): This test is testing internal implementation. In all the cases here, there may not even be a path available because the volume is not even mounted. Consider removing this test.
func (s *DockerExternalVolumeSuite) TestExternalVolumeDriverPathCalls(c *testing.T) {
	s.d.Start(c)
	assert.Equal(c, s.ec.paths, 0)

	out, err := s.d.Cmd("volume", "create", "test", "--driver=test-external-volume-driver")
	assert.NilError(c, err, out)
	assert.Equal(c, s.ec.paths, 0)

	out, err = s.d.Cmd("volume", "ls")
	assert.NilError(c, err, out)
	assert.Equal(c, s.ec.paths, 0)
}

func (s *DockerExternalVolumeSuite) TestExternalVolumeDriverMountID(c *testing.T) {
	ctx := testutil.GetContext(c)
	s.d.StartWithBusybox(ctx, c)

	out, err := s.d.Cmd("run", "--rm", "-v", "external-volume-test:/tmp/external-volume-test", "--volume-driver", volumePluginName, "busybox:latest", "cat", "/tmp/external-volume-test/test")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.TrimSpace(out) != "")
}

// Check that VolumeDriver.Capabilities gets called, and only called once
func (s *DockerExternalVolumeSuite) TestExternalVolumeDriverCapabilities(c *testing.T) {
	s.d.Start(c)
	assert.Equal(c, s.ec.caps, 0)

	for i := 0; i < 3; i++ {
		out, err := s.d.Cmd("volume", "create", "-d", volumePluginName, fmt.Sprintf("test%d", i))
		assert.NilError(c, err, out)
		assert.Equal(c, s.ec.caps, 1)
		out, err = s.d.Cmd("volume", "inspect", "--format={{.Scope}}", fmt.Sprintf("test%d", i))
		assert.NilError(c, err)
		assert.Equal(c, strings.TrimSpace(out), volume.GlobalScope)
	}
}

func (s *DockerExternalVolumeSuite) TestExternalVolumeDriverOutOfBandDelete(c *testing.T) {
	ctx := testutil.GetContext(c)
	driverName := stringid.GenerateRandomID()
	p := newVolumePlugin(c, driverName)
	defer p.Close()

	s.d.StartWithBusybox(ctx, c)

	out, err := s.d.Cmd("volume", "create", "-d", driverName, "--name", "test")
	assert.NilError(c, err, out)

	out, err = s.d.Cmd("volume", "create", "-d", "local", "--name", "test")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, is.Contains(out, "must be unique"))
	// simulate out of band volume deletion on plugin level
	delete(p.vols, "test")

	// test re-create with same driver
	out, err = s.d.Cmd("volume", "create", "-d", driverName, "--opt", "foo=bar", "--name", "test")
	assert.NilError(c, err, out)
	out, err = s.d.Cmd("volume", "inspect", "test")
	assert.NilError(c, err, out)

	var vs []volumetypes.Volume
	err = json.Unmarshal([]byte(out), &vs)
	assert.NilError(c, err)
	assert.Equal(c, len(vs), 1)
	assert.Equal(c, vs[0].Driver, driverName)
	assert.Assert(c, vs[0].Options != nil)
	assert.Equal(c, vs[0].Options["foo"], "bar")
	assert.Equal(c, vs[0].Driver, driverName)

	// simulate out of band volume deletion on plugin level
	delete(p.vols, "test")

	// test create with different driver
	out, err = s.d.Cmd("volume", "create", "-d", "local", "--name", "test")
	assert.NilError(c, err, out)

	out, err = s.d.Cmd("volume", "inspect", "test")
	assert.NilError(c, err, out)
	vs = nil
	err = json.Unmarshal([]byte(out), &vs)
	assert.NilError(c, err)
	assert.Equal(c, len(vs), 1)
	assert.Equal(c, len(vs[0].Options), 0)
	assert.Equal(c, vs[0].Driver, "local")
}

func (s *DockerExternalVolumeSuite) TestExternalVolumeDriverUnmountOnMountFail(c *testing.T) {
	ctx := testutil.GetContext(c)
	s.d.StartWithBusybox(ctx, c)
	s.d.Cmd("volume", "create", "-d", "test-external-volume-driver", "--opt=invalidOption=1", "--name=testumount")

	out, _ := s.d.Cmd("run", "-v", "testumount:/foo", "busybox", "true")
	assert.Equal(c, s.ec.unmounts, 0, out)
	out, _ = s.d.Cmd("run", "-w", "/foo", "-v", "testumount:/foo", "busybox", "true")
	assert.Equal(c, s.ec.unmounts, 0, out)
}

func (s *DockerExternalVolumeSuite) TestExternalVolumeDriverUnmountOnCp(c *testing.T) {
	ctx := testutil.GetContext(c)
	s.d.StartWithBusybox(ctx, c)
	s.d.Cmd("volume", "create", "-d", "test-external-volume-driver", "--name=test")

	out, _ := s.d.Cmd("run", "-d", "--name=test", "-v", "test:/foo", "busybox", "/bin/sh", "-c", "touch /test && top")
	assert.Equal(c, s.ec.mounts, 1, out)

	out, _ = s.d.Cmd("cp", "test:/test", "/tmp/test")
	assert.Equal(c, s.ec.mounts, 2, out)
	assert.Equal(c, s.ec.unmounts, 1, out)

	out, _ = s.d.Cmd("kill", "test")
	assert.Equal(c, s.ec.unmounts, 2, out)
}

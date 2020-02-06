package providers

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/shipyard-run/shipyard/pkg/clients/mocks"
	"github.com/shipyard-run/shipyard/pkg/config"
	"github.com/shipyard-run/shipyard/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// setupClusterMocks sets up a happy path for mocks
func setupClusterMocks() (*mocks.MockContainerTasks, *mocks.MockKubernetes, func()) {
	md := &mocks.MockContainerTasks{}
	md.On("FindContainerIDs", mock.Anything, mock.Anything).Return(nil, nil)
	md.On("CreateVolume", mock.Anything, mock.Anything).Return("123", nil)
	md.On("CreateContainer", mock.Anything).Return("containerid", nil)
	md.On("ContainerLogs", mock.Anything, true, true).Return(
		ioutil.NopCloser(bytes.NewBufferString("Running kubelet")),
		nil,
	)
	md.On("CopyFromContainer", mock.Anything, mock.Anything, mock.Anything).Return(nil)

	// set the home folder to a temp folder
	tmpDir, _ := ioutil.TempDir("", "")
	currentHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)

	// write the kubeconfi
	_, destPath, _ := CreateKubeConfigPath(clusterConfig.Name)
	kcf, err := os.Create(destPath)
	if err != nil {
		panic(err)
	}
	kcf.WriteString(kubeconfig)
	kcf.Close()

	mk := &mocks.MockKubernetes{}
	mk.Mock.On("SetConfig", mock.Anything).Return(nil)

	return md, mk, func() {
		os.Setenv("HOME", currentHome)
		os.RemoveAll(tmpDir)
	}
}

func TestClusterK3ErrorsWhenUnableToLookupIDs(t *testing.T) {
	md := &mocks.MockContainerTasks{}
	md.On("FindContainerIDs", mock.Anything, mock.Anything).Return(nil, fmt.Errorf("boom"))

	mk := &mocks.MockKubernetes{}
	p := NewCluster(&clusterConfig, md, mk, hclog.NewNullLogger())

	err := p.Create()
	assert.Error(t, err)
}

func TestClusterK3ErrorsWhenClusterExists(t *testing.T) {
	md := &mocks.MockContainerTasks{}
	md.On("FindContainerIDs", mock.Anything, mock.Anything).Return([]string{"abc"}, nil)

	mk := &mocks.MockKubernetes{}
	p := NewCluster(&clusterConfig, md, mk, hclog.NewNullLogger())

	err := p.Create()
	assert.Error(t, err)
}

func TestClusterK3CreatesANewVolume(t *testing.T) {
	md, mk, cleanup := setupClusterMocks()
	defer cleanup()

	p := NewCluster(&clusterConfig, md, mk, hclog.NewNullLogger())

	err := p.Create()
	assert.NoError(t, err)
	md.AssertCalled(t, "CreateVolume", clusterConfig.Name)
}

func TestClusterK3FailsWhenUnableToCreatesANewVolume(t *testing.T) {
	md, mk, cleanup := setupClusterMocks()
	defer cleanup()

	removeOn(&md.Mock, "CreateVolume")
	md.On("CreateVolume", mock.Anything, mock.Anything).Return("", fmt.Errorf("boom"))

	p := NewCluster(&clusterConfig, md, mk, hclog.NewNullLogger())

	err := p.Create()
	assert.Error(t, err)
	md.AssertCalled(t, "CreateVolume", clusterConfig.Name)
}

func TestClusterK3CreatesAServer(t *testing.T) {
	md, mk, cleanup := setupClusterMocks()
	defer cleanup()

	p := NewCluster(&clusterConfig, md, mk, hclog.NewNullLogger())

	err := p.Create()
	assert.NoError(t, err)

	params := getCalls(&md.Mock, "CreateContainer")[0].Arguments[0].(config.Container)

	// validate the basic details for the server container
	assert.Contains(t, params.Name, "server")
	assert.Contains(t, params.Image.Name, "rancher")
	assert.Equal(t, &clusterNetwork, params.NetworkRef)
	assert.True(t, params.Privileged)

	// validate that the volume is correctly set
	assert.Equal(t, "123", params.Volumes[0].Source)
	assert.Equal(t, "/images", params.Volumes[0].Destination)
	assert.Equal(t, "volume", params.Volumes[0].Type)

	// validate the API port is set
	assert.GreaterOrEqual(t, params.Ports[0].Local, 64000)
	assert.GreaterOrEqual(t, params.Ports[0].Local, params.Ports[0].Host)
	assert.Equal(t, "tcp", params.Ports[0].Protocol)

	// validate the command
	assert.Equal(t, "server", params.Command[0])
	assert.Contains(t, params.Command[1], strconv.Itoa(params.Ports[0].Local))
	assert.Contains(t, params.Command[2], "traefik")
}

func TestClusterK3sErrorsIfServerNOTStart(t *testing.T) {
	md, mk, cleanup := setupClusterMocks()
	defer cleanup()

	removeOn(&md.Mock, "ContainerLogs")
	md.On("ContainerLogs", mock.Anything, true, true).Return(
		ioutil.NopCloser(bytes.NewBufferString("Not running")),
		nil,
	)

	p := NewCluster(&clusterConfig, md, mk, hclog.NewNullLogger())
	startTimeout = 10 * time.Millisecond // reset the startTimeout, do not want to wait 120s

	err := p.Create()
	assert.Error(t, err)
}

func TestClusterK3sDownloadsConfig(t *testing.T) {
	md, mk, cleanup := setupClusterMocks()
	defer cleanup()

	p := NewCluster(&clusterConfig, md, mk, hclog.NewNullLogger())

	err := p.Create()
	assert.NoError(t, err)

	_, destPath, _ := CreateKubeConfigPath(clusterConfig.Name)
	params := getCalls(&md.Mock, "CopyFromContainer")[0].Arguments
	assert.Equal(t, "containerid", params.String(0))
	assert.Equal(t, "/output/kubeconfig.yaml", params.String(1))
	assert.Equal(t, destPath, params.String(2))
}

func TestClusterK3sRaisesErrorWhenUnableToDownloadConfig(t *testing.T) {
	md, mk, cleanup := setupClusterMocks()
	defer cleanup()

	removeOn(&md.Mock, "CopyFromContainer")
	md.On("CopyFromContainer", mock.Anything, mock.Anything, mock.Anything).Return(fmt.Errorf("boom"))

	p := NewCluster(&clusterConfig, md, mk, hclog.NewNullLogger())

	err := p.Create()
	assert.Error(t, err)
}

func TestClusterK3sCreatesDockerConfig(t *testing.T) {
	md, mk, cleanup := setupClusterMocks()
	defer cleanup()

	p := NewCluster(&clusterConfig, md, mk, hclog.NewNullLogger())

	err := p.Create()
	assert.NoError(t, err)

	// check the kubeconfig file for docker uses a network ip not localhost

	// check file has been written
	_, _, dockerPath := CreateKubeConfigPath(clusterConfig.Name)
	f, err := os.Open(dockerPath)
	assert.NoError(t, err)
	defer f.Close()

	// check file contains docker ip
	d, err := ioutil.ReadAll(f)
	assert.NoError(t, err)
	assert.Contains(t, string(d), fmt.Sprintf("server.%s", utils.FQDN(clusterConfig.Name, clusterConfig.NetworkRef.Name)))
}

func TestClusterK3sCreatesKubeClient(t *testing.T) {
	md, mk, cleanup := setupClusterMocks()
	defer cleanup()

	p := NewCluster(&clusterConfig, md, mk, hclog.NewNullLogger())

	err := p.Create()
	assert.NoError(t, err)
	mk.AssertCalled(t, "SetConfig", mock.Anything)
}

func TestClusterK3sErrorsWhenFailedToCreateKubeClient(t *testing.T) {
	md, mk, cleanup := setupClusterMocks()
	defer cleanup()

	removeOn(&mk.Mock, "SetConfig")
	mk.Mock.On("SetConfig", mock.Anything).Return(fmt.Errorf("boom"))

	startTimeout = 10 * time.Millisecond

	p := NewCluster(&clusterConfig, md, mk, hclog.NewNullLogger())

	err := p.Create()
	assert.Error(t, err)
}

func TestClusterK3sWaitsForPods(t *testing.T) {
	md, mk, cleanup := setupClusterMocks()
	defer cleanup()

	p := NewCluster(&clusterConfig, md, mk, hclog.NewNullLogger())

	err := p.Create()
	assert.NoError(t, err)
}

var clusterNetwork = config.Network{Name: "cloud"}

var clusterConfig = config.Cluster{
	Name:       "test",
	Driver:     "k3s",
	NetworkRef: &clusterNetwork,
}

var kubeconfig = `
apiVersion: v1
clusters:
- cluster:
   certificate-authority-data: LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSUJWakNCL3FBREFnRUNBZ0VBTUFvR0NDcUdTTTQ5QkFNQ01DTXhJVEFmQmdOVkJBTU1HR3N6Y3kxelpYSjIKWlhJdFkyRk
FNVFUzTlRrNE1qVTNNakFlRncweE9URXlNVEF4TWpVMk1USmFGdzB5T1RFeU1EY3hNalUyTVRKYQpNQ014SVRBZkJnTlZCQU1NR0dzemN5MXpaWEoyWlhJdFkyRkFNVFUzTlRrNE1qVTNNakJaTUJNR0J5cUdTTTQ5CkFn
RUdDQ3FHU000OUF3RUhBMElBQkhSblYydVliRU53eTlROGkxd2J6ZjQ2NytGdzV2LzRBWVQ2amM4dXorM00KTmRrZEwwd0RhNGM3Y1ByOUFXM1N0ZVRYSDNtNE9mRStJYTE3L1liaDFqR2pJekFoTUE0R0ExVWREd0VCL3
dRRQpBd0lDcERBUEJnTlZIUk1CQWY4RUJUQURBUUgvTUFvR0NDcUdTTTQ5QkFNQ0EwY0FNRVFDSUhFYlZwbUkzbjZwCnQrYlhKaWlFK1hiRm5XUFhtYm40OFZuNmtkYkdPM3daQWlCRDNyUjF5RjQ5R0piZmVQeXBsREdC
K3lkNVNQOEUKUmQ4OGxRWW9oRnV2enc9PQotLS0tLUVORCBDRVJUSUZJQ0FURS0tLS0tCg==
    server: https://127.0.0.1:64674
`

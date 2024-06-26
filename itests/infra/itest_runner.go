//go:build itest

package infra

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/jfrog/jfrog-cli-platform-services/cli"
	"github.com/jfrog/jfrog-cli-platform-services/model"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	coreCommans "github.com/jfrog/jfrog-cli-core/v2/common/commands"
	"github.com/jfrog/jfrog-cli-core/v2/plugins"
	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/jfrog/jfrog-cli-core/v2/utils/coreutils"
	"github.com/stretchr/testify/require"

	"github.com/jfrog/jfrog-cli-platform-services/commands"
)

type TestFunction func(it *Test)

type TestDefinition struct {
	Name          string
	Only          bool
	Skip          bool
	Input         string
	CaptureOutput bool
	Test          TestFunction
}

type Test struct {
	ServerId    string
	AccessToken string
	dataDir     string
	platformUrl string
	output      *bytes.Buffer
	t           *testing.T
}

var runPlugin = plugins.RunCliWithPlugin(cli.GetPlatformServicesApp())

func RunITests(tests []TestDefinition, t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	defer http.DefaultClient.CloseIdleConnections()

	var containsOnly *TestDefinition
	for _, tt := range tests {
		if tt.Only {
			containsOnly = &tt //nolint:exportability // We are good with pointing to 'tt' as 'break' is used.
			break
		}
	}

	if containsOnly != nil {
		tests = []TestDefinition{*containsOnly}
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			if tt.Skip {
				t.Skip("Skipping ", tt.Name)
			}
			runTest(t, tt)
		})
	}
}

func runTest(t *testing.T, testSpec TestDefinition) {

	homeDir := createTestHomeDir(t)

	// Setup cli home for tests
	err := os.Setenv(coreutils.HomeDir, homeDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		err := os.Unsetenv(coreutils.HomeDir)
		if err != nil {
			t.Logf("Error unsetting env '%s': %+v", coreutils.HomeDir, err)
		}
	})

	serverId := uuid.NewString()

	platformUrl := os.Getenv("JF_PLATFORM_URL")
	require.NotEmpty(t, platformUrl, "No platform URL provided, please set JF_PLATFORM_URL env var")

	accessToken := os.Getenv("JF_PLATFORM_ACCESS_TOKEN")
	require.NotEmpty(t, accessToken, "No platform token provided, please set JF_PLATFORM_ACCESS_TOKEN env var")

	// Generates a server config
	configCmd := coreCommans.NewConfigCommand(coreCommans.AddOrEdit, serverId)
	configCmd.SetInteractive(false)
	configCmd.SetMakeDefault(true)
	configCmd.SetEncPassword(false)
	configCmd.SetDetails(&config.ServerDetails{
		Url:         platformUrl,
		AccessToken: accessToken,
	})
	require.NoError(t, configCmd.Run())

	it := &Test{
		t:           t,
		ServerId:    serverId,
		AccessToken: accessToken,
		dataDir:     homeDir,
		platformUrl: platformUrl,
	}

	if testSpec.Input != "" {
		commands.SetCliIn(bytes.NewReader([]byte(testSpec.Input)))
		t.Cleanup(func() {
			commands.SetCliIn(os.Stdin)
		})
	}

	if testSpec.CaptureOutput {
		var newOutput bytes.Buffer
		commands.SetCliOut(&newOutput)
		t.Cleanup(func() {
			commands.SetCliOut(os.Stdout)
		})
		it.output = &newOutput
	}

	testSpec.Test(it)
}

func (it *Test) PrepareWorkerTestDir() (string, string) {
	dir, err := os.MkdirTemp("", "worker-*-init")
	require.NoError(it, err)

	it.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})

	oldPwd, err := os.Getwd()
	require.NoError(it, err)

	err = os.Chdir(dir)
	require.NoError(it, err)

	it.Cleanup(func() {
		require.NoError(it, os.Chdir(oldPwd))
	})

	workerName := path.Base(dir)

	return dir, workerName
}

func createTestHomeDir(t *testing.T) string {
	_, filename, _, _ := runtime.Caller(0)
	dir, err := os.MkdirTemp(filepath.Dir(filename), ".itest-*-no-git")
	require.NoError(t, err)
	t.Cleanup(func() {
		err := os.RemoveAll(dir)
		if err != nil {
			t.Logf("Cannot remove test dir: %+v", err)
		}
	})
	return dir
}

func (it *Test) RunCommand(args ...string) error {
	oldArgs := os.Args
	defer func() {
		os.Args = oldArgs
	}()
	os.Args = args
	return runPlugin()
}

func (it *Test) CapturedOutput() []byte {
	if it.output != nil {
		return it.output.Bytes()
	}
	return nil
}

func (it *Test) GetAllWorkers() []*model.WorkerDetails {
	ctx, cancelCtx := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelCtx()

	response := struct {
		Workers []*model.WorkerDetails `json:"workers"`
	}{}

	it.NewHttpRequestWithContext(ctx).
		WithAccessToken().
		Get("/worker/api/v1/workers").
		Do().
		AsObject(&response)

	return response.Workers
}

func (it *Test) CreateWorker(createRequest *model.WorkerDetails) {
	ctx, cancelCtx := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelCtx()

	jsonBytes, err := json.Marshal(createRequest)
	require.NoError(it, err)

	it.NewHttpRequestWithContext(ctx).
		WithAccessToken().
		WithJsonBytes(jsonBytes).
		Post("/worker/api/v1/workers").
		Do().
		IsCreated()
}

func (it *Test) DeleteWorker(workerKey string) {
	ctx, cancelCtx := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelCtx()

	it.Logf("Deleting worker '%s'", workerKey)

	status := it.NewHttpRequestWithContext(ctx).
		WithAccessToken().
		Delete("/worker/api/v1/workers/" + workerKey).
		Do().
		StatusCode()

	if status != http.StatusNoContent {
		it.Logf("Delete worker '%s' failed with status %d", workerKey, status)
	} else {
		it.Logf("Deleted worker '%s'", workerKey)
	}
}

func (it *Test) DeleteAllWorkers() {
	for _, wk := range it.GetAllWorkers() {
		it.DeleteWorker(wk.Key)
	}
}

func (it *Test) Errorf(format string, args ...interface{}) {
	it.t.Errorf(format, args...)
}

func (it *Test) Logf(format string, args ...interface{}) {
	it.t.Logf(format, args...)
}

func (it *Test) FailNow() {
	it.t.FailNow()
}

func (it *Test) Skip() {
	it.t.Skip()
}

func (it *Test) Cleanup(f func()) {
	it.t.Cleanup(f)
}

func (it *Test) SkipBecause(reason string) {
	it.t.Skipf(reason)
}

func (it *Test) Helper() {
	it.t.Helper()
}

func (it *Test) Run(name string, f func(t *Test)) bool {
	return it.t.Run(name, func(t *testing.T) {
		f(&Test{t: t, ServerId: it.ServerId, AccessToken: it.AccessToken, dataDir: it.dataDir, output: it.output})
	})
}

func (it *Test) NewHttpRequest() *HttpRequest {
	return &HttpRequest{
		it:      it,
		url:     it.platformUrl,
		headers: make(map[string]string),
	}
}

func (it *Test) NewHttpRequestWithContext(ctx context.Context) *HttpRequest {
	r := &HttpRequest{
		it:         it,
		url:        it.platformUrl,
		headers:    make(map[string]string),
		reqContext: ctx,
	}
	return r
}

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"e2b-local/internal/e2bapi"

	"github.com/docker/docker/errdefs"
	sdkvolume "github.com/superduck-ai/e2b-go-sdk/volume"
)

func newTestApp(t *testing.T, cfg Config) http.Handler {
	t.Helper()
	app, err := NewAppWithRuntime(cfg, log.New(io.Discard, "", 0), &recordingRuntime{})
	if err != nil {
		t.Fatalf("create test app: %v", err)
	}
	return app
}

func TestAppExplicitlyImplementsGeneratedOpenAPIInterface(t *testing.T) {
	interfaceMethods := generatedServerInterfaceMethods(t)
	appMethods := explicitAppMethods(t)
	unimplementedMethods := map[string]bool{
		"DeleteAccessTokensAccessTokenID": true,
		"DeleteApiKeysApiKeyID":           true,
		"GetApiKeys":                      true,
		"PatchApiKeysApiKeyID":            true,
		"PostAccessTokens":                true,
		"PostApiKeys":                     true,
		"PutSandboxesSandboxIDNetwork":    true,
	}

	var missing []string
	for _, method := range interfaceMethods {
		if unimplementedMethods[method] {
			continue
		}
		if !appMethods[method] {
			missing = append(missing, method)
		}
	}

	if len(missing) > 0 {
		t.Fatalf("App must explicitly implement generated ServerInterface methods, missing: %s", strings.Join(missing, ", "))
	}
}

func TestCreateAndDeleteSandbox(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"base"}`))
	createRec := httptest.NewRecorder()

	app.ServeHTTP(createRec, createReq)

	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	var sandbox SandboxResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &sandbox); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	if sandbox.SandboxID == "" {
		t.Fatal("expected sandboxID")
	}

	if sandbox.TemplateID != "base" {
		t.Fatalf("expected templateID base, got %q", sandbox.TemplateID)
	}

	if sandbox.EnvdVersion == "" {
		t.Fatal("expected envdVersion")
	}

	if sandbox.EnvdURL == "" {
		t.Fatal("expected envdURL")
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/sandboxes/"+sandbox.SandboxID, nil)
	deleteRec := httptest.NewRecorder()

	app.ServeHTTP(deleteRec, deleteReq)

	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("expected delete status %d, got %d: %s", http.StatusNoContent, deleteRec.Code, deleteRec.Body.String())
	}
}

func TestSandboxLifecycleCallsRuntime(t *testing.T) {
	runtime := &recordingRuntime{}
	app, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"base","envVars":{"A":"1"}}`))
	createRec := httptest.NewRecorder()

	app.ServeHTTP(createRec, createReq)

	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	var sandbox SandboxResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &sandbox); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	if len(runtime.createCalls) != 1 {
		t.Fatalf("expected 1 runtime create call, got %d", len(runtime.createCalls))
	}

	if runtime.createCalls[0].SandboxID != sandbox.SandboxID {
		t.Fatalf("expected runtime sandbox id %q, got %q", sandbox.SandboxID, runtime.createCalls[0].SandboxID)
	}

	if sandbox.EnvdURL != "http://127.0.0.1:50000" {
		t.Fatalf("expected envdURL from runtime, got %q", sandbox.EnvdURL)
	}

	if runtime.createCalls[0].EnvVars["A"] != "1" {
		t.Fatalf("expected env var passed to runtime, got %#v", runtime.createCalls[0].EnvVars)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/sandboxes/"+sandbox.SandboxID, nil)
	deleteRec := httptest.NewRecorder()

	app.ServeHTTP(deleteRec, deleteReq)

	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("expected delete status %d, got %d: %s", http.StatusNoContent, deleteRec.Code, deleteRec.Body.String())
	}

	if len(runtime.deleteInfos) != 1 {
		t.Fatalf("expected 1 runtime delete call, got %d", len(runtime.deleteInfos))
	}

	if runtime.deleteInfos[0].ContainerID != "ctr-"+sandbox.SandboxID {
		t.Fatalf("expected runtime delete to receive container id for sandbox, got %#v", runtime.deleteInfos[0])
	}
}

func TestGetSandboxPortReturnsAdvertisedHostURL(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Traffic.AdvertisedHost = "192.0.2.10"
	runtime := &recordingRuntime{
		createRuntimeInfo: SandboxRuntimeInfo{
			EnvdURL:       "http://127.0.0.1:50000",
			ContainerID:   "ctr-test",
			ContainerName: "e2b-envd-test",
			HostPort:      "50000",
			PublishedPorts: []SandboxPortMapping{{
				ContainerPort: 5000,
				HostIP:        "0.0.0.0",
				HostPort:      38123,
				Protocol:      "tcp",
			}},
		},
	}
	app, err := NewAppWithRuntime(cfg, log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	created := createSandboxForTest(t, app, "base", http.StatusCreated)

	req := httptest.NewRequest(http.MethodGet, "/sandboxes/"+created.SandboxID+"/ports/5000", nil)
	rec := httptest.NewRecorder()

	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected port lookup status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var response SandboxPortResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode port response: %v", err)
	}

	if response.ContainerPort != 5000 || response.Host != "192.0.2.10" || response.HostPort != 38123 {
		t.Fatalf("unexpected port response: %#v", response)
	}
	if response.URL != "http://192.0.2.10:38123" {
		t.Fatalf("expected HTTP URL with advertised host, got %q", response.URL)
	}
	if response.WSURL != "ws://192.0.2.10:38123" {
		t.Fatalf("expected WS URL with advertised host, got %q", response.WSURL)
	}
}

func TestGetSandboxPortReturnsNotFoundForUnpublishedPort(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Traffic.AdvertisedHost = "192.0.2.10"
	app := newTestApp(t, cfg)

	created := createSandboxForTest(t, app, "base", http.StatusCreated)

	req := httptest.NewRequest(http.MethodGet, "/sandboxes/"+created.SandboxID+"/ports/5000", nil)
	rec := httptest.NewRecorder()

	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected unpublished port status %d, got %d: %s", http.StatusNotFound, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "docker.published_ports") {
		t.Fatalf("expected helpful unpublished port error, got %s", rec.Body.String())
	}
}

func TestPauseAndConnectCallRuntime(t *testing.T) {
	runtime := &recordingRuntime{}
	app, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"base"}`))
	createRec := httptest.NewRecorder()

	app.ServeHTTP(createRec, createReq)

	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	var sandbox SandboxResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &sandbox); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	pauseReq := httptest.NewRequest(http.MethodPost, "/sandboxes/"+sandbox.SandboxID+"/pause", nil)
	pauseRec := httptest.NewRecorder()

	app.ServeHTTP(pauseRec, pauseReq)

	if pauseRec.Code != http.StatusNoContent {
		t.Fatalf("expected pause status %d, got %d: %s", http.StatusNoContent, pauseRec.Code, pauseRec.Body.String())
	}

	if len(runtime.pauseInfos) != 1 || runtime.pauseInfos[0].ContainerID != "ctr-"+sandbox.SandboxID {
		t.Fatalf("expected runtime pause to receive sandbox container, got %#v", runtime.pauseInfos)
	}

	connectReq := httptest.NewRequest(http.MethodPost, "/sandboxes/"+sandbox.SandboxID+"/connect", bytes.NewBufferString(`{"timeout":300}`))
	connectRec := httptest.NewRecorder()

	app.ServeHTTP(connectRec, connectReq)

	if connectRec.Code != http.StatusCreated {
		t.Fatalf("expected connect status %d, got %d: %s", http.StatusCreated, connectRec.Code, connectRec.Body.String())
	}

	if len(runtime.resumeInfos) != 1 || runtime.resumeInfos[0].ContainerID != "ctr-"+sandbox.SandboxID {
		t.Fatalf("expected runtime resume to receive sandbox container, got %#v", runtime.resumeInfos)
	}
}

func TestDeleteMissingSandbox(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	req := httptest.NewRequest(http.MethodDelete, "/sandboxes/sbx_missing", nil)
	rec := httptest.NewRecorder()

	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, rec.Code)
	}

	var errResp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}

	if errResp.Code != http.StatusNotFound {
		t.Fatalf("expected error code %d, got %d", http.StatusNotFound, errResp.Code)
	}
}

func TestListSandboxes(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"base","metadata":{"source":"test"}}`))
	createRec := httptest.NewRecorder()

	app.ServeHTTP(createRec, createReq)

	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/v2/sandboxes", nil)
	listRec := httptest.NewRecorder()

	app.ServeHTTP(listRec, listReq)

	if listRec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d: %s", http.StatusOK, listRec.Code, listRec.Body.String())
	}

	var sandboxes []ListedSandboxResponse
	if err := json.Unmarshal(listRec.Body.Bytes(), &sandboxes); err != nil {
		t.Fatalf("decode list response: %v", err)
	}

	if len(sandboxes) != 1 {
		t.Fatalf("expected 1 sandbox, got %d", len(sandboxes))
	}

	if sandboxes[0].TemplateID != "base" {
		t.Fatalf("expected templateID base, got %q", sandboxes[0].TemplateID)
	}

	if sandboxes[0].State != "running" {
		t.Fatalf("expected state running, got %q", sandboxes[0].State)
	}

	if sandboxes[0].Metadata["source"] != "test" {
		t.Fatalf("expected metadata source test, got %#v", sandboxes[0].Metadata)
	}
}

func TestListSandboxesReconcilesMissingRuntimeSandbox(t *testing.T) {
	runtime := &recordingRuntime{}
	app, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"base"}`))
	createRec := httptest.NewRecorder()
	app.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	var created SandboxResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	runtime.inspectResults = map[string]SandboxRuntimeInspection{
		"ctr-" + created.SandboxID: {Exists: false},
	}

	listReq := httptest.NewRequest(http.MethodGet, "/v2/sandboxes", nil)
	listRec := httptest.NewRecorder()
	app.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d: %s", http.StatusOK, listRec.Code, listRec.Body.String())
	}

	var sandboxes []listedSandboxAPIResponse
	if err := json.Unmarshal(listRec.Body.Bytes(), &sandboxes); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(sandboxes) != 0 {
		t.Fatalf("expected missing runtime sandbox to be removed from list, got %#v", sandboxes)
	}
	if len(runtime.inspectCalls) == 0 {
		t.Fatal("expected runtime inspector to be called")
	}

	getReq := httptest.NewRequest(http.MethodGet, "/sandboxes/"+created.SandboxID, nil)
	getRec := httptest.NewRecorder()
	app.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusNotFound {
		t.Fatalf("expected get status %d after reconcile, got %d: %s", http.StatusNotFound, getRec.Code, getRec.Body.String())
	}
}

func TestReconcileMissingSandboxDeletesRuntimeContainer(t *testing.T) {
	runtime := &recordingRuntime{}
	app, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"base"}`))
	createRec := httptest.NewRecorder()
	app.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	var created SandboxResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	runtime.inspectResults = map[string]SandboxRuntimeInspection{
		"ctr-" + created.SandboxID: {Exists: false},
	}

	listReq := httptest.NewRequest(http.MethodGet, "/v2/sandboxes", nil)
	listRec := httptest.NewRecorder()
	app.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d: %s", http.StatusOK, listRec.Code, listRec.Body.String())
	}

	if len(runtime.deleteInfos) != 1 {
		t.Fatalf("expected runtime DeleteSandbox to be called for missing container, got %d calls", len(runtime.deleteInfos))
	}
	if runtime.deleteInfos[0].ContainerID != "ctr-"+created.SandboxID {
		t.Fatalf("expected runtime delete of container %q, got %q", "ctr-"+created.SandboxID, runtime.deleteInfos[0].ContainerID)
	}
}

func TestReconcileMissingSandboxRemovesMappingWhenRuntimeDeleteFails(t *testing.T) {
	runtime := &recordingRuntime{
		deleteSandboxErr: errors.New("runtime delete failed"),
	}
	handler, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	app := handler.(*App)

	createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"base"}`))
	createRec := httptest.NewRecorder()
	app.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	var created SandboxResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	runtime.inspectResults = map[string]SandboxRuntimeInspection{
		"ctr-" + created.SandboxID: {Exists: false},
	}

	listReq := httptest.NewRequest(http.MethodGet, "/v2/sandboxes", nil)
	listRec := httptest.NewRecorder()
	app.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d: %s", http.StatusOK, listRec.Code, listRec.Body.String())
	}

	if len(runtime.deleteInfos) != 1 {
		t.Fatalf("expected one best-effort runtime delete, got %d", len(runtime.deleteInfos))
	}
	if _, exists := app.store.Get(created.SandboxID); exists {
		t.Fatal("expected missing runtime sandbox mapping to be removed despite cleanup failure")
	}
}

func TestDeleteSandboxRuntimeFailureReleasesLock(t *testing.T) {
	runtime := &recordingRuntime{
		deleteSandboxErr: errors.New("runtime delete failed"),
	}
	handler, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	app := handler.(*App)

	createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"base"}`))
	createRec := httptest.NewRecorder()
	app.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	var created SandboxResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/sandboxes/"+created.SandboxID, nil)
	deleteRec := httptest.NewRecorder()
	app.ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusInternalServerError {
		t.Fatalf("expected delete status %d, got %d: %s", http.StatusInternalServerError, deleteRec.Code, deleteRec.Body.String())
	}

	_, exists := app.store.Get(created.SandboxID)
	if !exists {
		t.Fatal("expected sandbox mapping to remain after required runtime delete failure")
	}

	timeoutReq := httptest.NewRequest(http.MethodPost, "/sandboxes/"+created.SandboxID+"/timeout", bytes.NewBufferString(`{"timeout":30}`))
	timeoutRec := httptest.NewRecorder()
	app.ServeHTTP(timeoutRec, timeoutReq)
	if timeoutRec.Code != http.StatusNoContent {
		t.Fatalf("expected sandbox to accept timeout after failed delete, got %d: %s", timeoutRec.Code, timeoutRec.Body.String())
	}
}

func TestListSandboxesReconcilesRuntimePausedState(t *testing.T) {
	runtime := &recordingRuntime{}
	app, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"base"}`))
	createRec := httptest.NewRecorder()
	app.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	var created SandboxResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	runtime.inspectResults = map[string]SandboxRuntimeInspection{
		"ctr-" + created.SandboxID: {
			Exists: true,
			State:  string(e2bapi.Paused),
			Info: SandboxRuntimeInfo{
				ContainerID:   "ctr-" + created.SandboxID,
				ContainerName: "e2b-envd-" + created.SandboxID,
				EnvdURL:       "http://127.0.0.1:50001",
				HostPort:      "50001",
			},
		},
	}

	listReq := httptest.NewRequest(http.MethodGet, "/v2/sandboxes?state=paused", nil)
	listRec := httptest.NewRecorder()
	app.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d: %s", http.StatusOK, listRec.Code, listRec.Body.String())
	}

	var sandboxes []listedSandboxAPIResponse
	if err := json.Unmarshal(listRec.Body.Bytes(), &sandboxes); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(sandboxes) != 1 || sandboxes[0].SandboxID != created.SandboxID || sandboxes[0].State != e2bapi.Paused {
		t.Fatalf("expected reconciled paused sandbox, got %#v", sandboxes)
	}
	if sandboxes[0].EnvdURL != "http://127.0.0.1:50001" {
		t.Fatalf("expected reconciled envdURL, got %q", sandboxes[0].EnvdURL)
	}
}

func TestConnectSandbox(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"base"}`))
	createRec := httptest.NewRecorder()

	app.ServeHTTP(createRec, createReq)

	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	var created SandboxResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	connectReq := httptest.NewRequest(http.MethodPost, "/sandboxes/"+created.SandboxID+"/connect", bytes.NewBufferString(`{"timeout":300}`))
	connectRec := httptest.NewRecorder()

	app.ServeHTTP(connectRec, connectReq)

	if connectRec.Code != http.StatusOK {
		t.Fatalf("expected connect status %d, got %d: %s", http.StatusOK, connectRec.Code, connectRec.Body.String())
	}

	var connected SandboxResponse
	if err := json.Unmarshal(connectRec.Body.Bytes(), &connected); err != nil {
		t.Fatalf("decode connect response: %v", err)
	}

	if connected.SandboxID != created.SandboxID {
		t.Fatalf("expected sandboxID %q, got %q", created.SandboxID, connected.SandboxID)
	}

	if connected.TemplateID != "base" {
		t.Fatalf("expected templateID base, got %q", connected.TemplateID)
	}
}

func TestCreateSandboxRejectsNegativeTimeout(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	req := httptest.NewRequest(
		http.MethodPost,
		"/sandboxes",
		bytes.NewBufferString(`{"templateID":"base","timeout":-1}`),
	)
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected negative create timeout status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
}

func TestGetSandboxDetail(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"base","metadata":{"source":"detail"},"allow_internet_access":true}`))
	createRec := httptest.NewRecorder()

	app.ServeHTTP(createRec, createReq)

	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	var created SandboxResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/sandboxes/"+created.SandboxID, nil)
	getRec := httptest.NewRecorder()

	app.ServeHTTP(getRec, getReq)

	if getRec.Code != http.StatusOK {
		t.Fatalf("expected get status %d, got %d: %s", http.StatusOK, getRec.Code, getRec.Body.String())
	}

	var detail e2bapi.SandboxDetail
	if err := json.Unmarshal(getRec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail response: %v", err)
	}
	if detail.SandboxID != created.SandboxID || detail.TemplateID != "base" {
		t.Fatalf("unexpected detail response: %#v", detail)
	}
	if detail.State != e2bapi.Running {
		t.Fatalf("expected running state, got %q", detail.State)
	}
	if detail.Metadata == nil || (*detail.Metadata)["source"] != "detail" {
		t.Fatalf("expected metadata source detail, got %#v", detail.Metadata)
	}
	if detail.AllowInternetAccess == nil || !*detail.AllowInternetAccess {
		t.Fatalf("expected allowInternetAccess true, got %#v", detail.AllowInternetAccess)
	}
}

func TestAppRestoresRuntimeSandboxesOnStartup(t *testing.T) {
	createdAt := time.Now().UTC().Add(-time.Minute)
	endAt := time.Now().UTC().Add(time.Hour)
	cfg := DefaultConfig()
	runtime := &restoringRuntime{
		restoreRecords: []SandboxRecord{
			{
				ID:         "sbx_restored",
				TemplateID: "base",
				Metadata:   map[string]string{"source": "restored"},
				EnvdURL:    "http://127.0.0.1:50000",
				RuntimeInfo: SandboxRuntimeInfo{
					EnvdURL:       "http://127.0.0.1:50000",
					ContainerID:   "ctr-restored",
					ContainerName: "e2b-envd-sbx_restored",
					HostPort:      "50000",
				},
				CreatedAt:            createdAt,
				EndAt:                endAt,
				State:                string(e2bapi.Running),
				InternetAccessPolicy: InternetAccessDenied,
			},
		},
	}
	app, err := NewAppWithRuntime(cfg, log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if runtime.restoreCalls != 1 {
		t.Fatalf("expected one restore call, got %d", runtime.restoreCalls)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/sandboxes/sbx_restored", nil)
	getRec := httptest.NewRecorder()
	app.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected restored sandbox status %d, got %d: %s", http.StatusOK, getRec.Code, getRec.Body.String())
	}

	var detail e2bapi.SandboxDetail
	if err := json.Unmarshal(getRec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode restored sandbox detail: %v", err)
	}
	if detail.SandboxID != "sbx_restored" || detail.TemplateID != "base" {
		t.Fatalf("unexpected restored sandbox detail: %#v", detail)
	}
	if detail.Metadata == nil || (*detail.Metadata)["source"] != "restored" {
		t.Fatalf("expected restored metadata, got %#v", detail.Metadata)
	}
	if detail.AllowInternetAccess == nil || *detail.AllowInternetAccess {
		t.Fatalf("expected restored allowInternetAccess false, got %#v", detail.AllowInternetAccess)
	}
}

func TestSetSandboxTimeoutUpdatesEndAt(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"base"}`))
	createRec := httptest.NewRecorder()
	app.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	var created SandboxResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	before := time.Now().UTC()
	timeoutReq := httptest.NewRequest(http.MethodPost, "/sandboxes/"+created.SandboxID+"/timeout", bytes.NewBufferString(`{"timeout":30}`))
	timeoutRec := httptest.NewRecorder()
	app.ServeHTTP(timeoutRec, timeoutReq)
	after := time.Now().UTC()
	if timeoutRec.Code != http.StatusNoContent {
		t.Fatalf("expected timeout status %d, got %d: %s", http.StatusNoContent, timeoutRec.Code, timeoutRec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/sandboxes/"+created.SandboxID, nil)
	getRec := httptest.NewRecorder()
	app.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected get status %d, got %d: %s", http.StatusOK, getRec.Code, getRec.Body.String())
	}

	var detail e2bapi.SandboxDetail
	if err := json.Unmarshal(getRec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail response: %v", err)
	}

	minEndAt := before.Add(29 * time.Second)
	maxEndAt := after.Add(31 * time.Second)
	if detail.EndAt.Before(minEndAt) || detail.EndAt.After(maxEndAt) {
		t.Fatalf("expected endAt near now+30s, got %s between %s and %s", detail.EndAt, minEndAt, maxEndAt)
	}
}

func TestExpiredSandboxIsDeletedDuringReconcile(t *testing.T) {
	runtime := &recordingRuntime{}
	app, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"base"}`))
	createRec := httptest.NewRecorder()
	app.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	var created SandboxResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	timeoutReq := httptest.NewRequest(http.MethodPost, "/sandboxes/"+created.SandboxID+"/timeout", bytes.NewBufferString(`{"timeout":0}`))
	timeoutRec := httptest.NewRecorder()
	app.ServeHTTP(timeoutRec, timeoutReq)
	if timeoutRec.Code != http.StatusNoContent {
		t.Fatalf("expected timeout status %d, got %d: %s", http.StatusNoContent, timeoutRec.Code, timeoutRec.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/v2/sandboxes", nil)
	listRec := httptest.NewRecorder()
	app.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d: %s", http.StatusOK, listRec.Code, listRec.Body.String())
	}

	var sandboxes []ListedSandboxResponse
	if err := json.Unmarshal(listRec.Body.Bytes(), &sandboxes); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(sandboxes) != 0 {
		t.Fatalf("expected expired sandbox to be removed from list, got %#v", sandboxes)
	}
	deadline := time.Now().Add(time.Second)
	for {
		if _, exists := app.(*App).store.Get(created.SandboxID); !exists {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for expired sandbox deletion")
		}
		time.Sleep(time.Millisecond)
	}
	if len(runtime.deleteInfos) != 1 || runtime.deleteInfos[0].ContainerID != "ctr-"+created.SandboxID {
		t.Fatalf("expected expired sandbox to be deleted by runtime, got %#v", runtime.deleteInfos)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/sandboxes/"+created.SandboxID, nil)
	getRec := httptest.NewRecorder()
	app.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusNotFound {
		t.Fatalf("expected get status %d after expiration, got %d: %s", http.StatusNotFound, getRec.Code, getRec.Body.String())
	}
}

func TestSetSandboxTimeoutValidatesRequest(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	negativeReq := httptest.NewRequest(http.MethodPost, "/sandboxes/sbx_missing/timeout", bytes.NewBufferString(`{"timeout":-1}`))
	negativeRec := httptest.NewRecorder()
	app.ServeHTTP(negativeRec, negativeReq)
	if negativeRec.Code != http.StatusBadRequest {
		t.Fatalf("expected negative timeout status %d, got %d: %s", http.StatusBadRequest, negativeRec.Code, negativeRec.Body.String())
	}

	missingReq := httptest.NewRequest(http.MethodPost, "/sandboxes/sbx_missing/timeout", bytes.NewBufferString(`{"timeout":0}`))
	missingRec := httptest.NewRecorder()
	app.ServeHTTP(missingRec, missingReq)
	if missingRec.Code != http.StatusNotFound {
		t.Fatalf("expected missing sandbox status %d, got %d: %s", http.StatusNotFound, missingRec.Code, missingRec.Body.String())
	}
}

func TestSandboxNetworkEndpointIsNotImplemented(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"base"}`))
	createRec := httptest.NewRecorder()
	app.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	var created SandboxResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	updateReq := httptest.NewRequest(http.MethodPut, "/sandboxes/"+created.SandboxID+"/network", bytes.NewBufferString(`{"denyOut":["0.0.0.0/0"]}`))
	updateRec := httptest.NewRecorder()
	app.ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusNotImplemented {
		t.Fatalf("expected network update status %d, got %d: %s", http.StatusNotImplemented, updateRec.Code, updateRec.Body.String())
	}
}

func TestRefreshSandboxExtendsEndAt(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"base","timeout":1}`))
	createRec := httptest.NewRecorder()
	app.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	var created SandboxResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	before := time.Now().UTC()
	refreshReq := httptest.NewRequest(http.MethodPost, "/sandboxes/"+created.SandboxID+"/refreshes", bytes.NewBufferString(`{"duration":30}`))
	refreshRec := httptest.NewRecorder()
	app.ServeHTTP(refreshRec, refreshReq)
	after := time.Now().UTC()
	if refreshRec.Code != http.StatusNoContent {
		t.Fatalf("expected refresh status %d, got %d: %s", http.StatusNoContent, refreshRec.Code, refreshRec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/sandboxes/"+created.SandboxID, nil)
	getRec := httptest.NewRecorder()
	app.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected get status %d, got %d: %s", http.StatusOK, getRec.Code, getRec.Body.String())
	}

	var detail e2bapi.SandboxDetail
	if err := json.Unmarshal(getRec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail response: %v", err)
	}

	minEndAt := before.Add(29 * time.Second)
	maxEndAt := after.Add(31 * time.Second)
	if detail.EndAt.Before(minEndAt) || detail.EndAt.After(maxEndAt) {
		t.Fatalf("expected endAt near now+30s, got %s between %s and %s", detail.EndAt, minEndAt, maxEndAt)
	}
}

func TestRefreshSandboxValidatesDuration(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	negativeReq := httptest.NewRequest(http.MethodPost, "/sandboxes/sbx_missing/refreshes", bytes.NewBufferString(`{"duration":-1}`))
	negativeRec := httptest.NewRecorder()
	app.ServeHTTP(negativeRec, negativeReq)
	if negativeRec.Code != http.StatusBadRequest {
		t.Fatalf("expected negative refresh status %d, got %d: %s", http.StatusBadRequest, negativeRec.Code, negativeRec.Body.String())
	}

	tooLongReq := httptest.NewRequest(http.MethodPost, "/sandboxes/sbx_missing/refreshes", bytes.NewBufferString(`{"duration":3601}`))
	tooLongRec := httptest.NewRecorder()
	app.ServeHTTP(tooLongRec, tooLongReq)
	if tooLongRec.Code != http.StatusBadRequest {
		t.Fatalf("expected long refresh status %d, got %d: %s", http.StatusBadRequest, tooLongRec.Code, tooLongRec.Body.String())
	}

	missingReq := httptest.NewRequest(http.MethodPost, "/sandboxes/sbx_missing/refreshes", bytes.NewBufferString(`{"duration":0}`))
	missingRec := httptest.NewRecorder()
	app.ServeHTTP(missingRec, missingReq)
	if missingRec.Code != http.StatusNotFound {
		t.Fatalf("expected missing sandbox status %d, got %d: %s", http.StatusNotFound, missingRec.Code, missingRec.Body.String())
	}
}

func TestSandboxMetrics(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"base"}`))
	createRec := httptest.NewRecorder()
	app.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	var created SandboxResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	metricsReq := httptest.NewRequest(http.MethodGet, "/sandboxes/"+created.SandboxID+"/metrics", nil)
	metricsRec := httptest.NewRecorder()
	app.ServeHTTP(metricsRec, metricsReq)
	if metricsRec.Code != http.StatusOK {
		t.Fatalf("expected metrics status %d, got %d: %s", http.StatusOK, metricsRec.Code, metricsRec.Body.String())
	}

	var metrics []e2bapi.SandboxMetric
	if err := json.Unmarshal(metricsRec.Body.Bytes(), &metrics); err != nil {
		t.Fatalf("decode metrics response: %v", err)
	}
	if len(metrics) != 1 {
		t.Fatalf("expected 1 metric, got %#v", metrics)
	}
	if metrics[0].CpuCount != 1 || metrics[0].MemTotal != 512*1024*1024 {
		t.Fatalf("unexpected metric values: %#v", metrics[0])
	}

	listReq := httptest.NewRequest(http.MethodGet, "/sandboxes/metrics?sandbox_ids="+created.SandboxID, nil)
	listRec := httptest.NewRecorder()
	app.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected list metrics status %d, got %d: %s", http.StatusOK, listRec.Code, listRec.Body.String())
	}

	var listed e2bapi.SandboxesWithMetrics
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list metrics response: %v", err)
	}
	if len(listed.Sandboxes) != 1 || listed.Sandboxes[created.SandboxID].CpuCount != 1 {
		t.Fatalf("expected sandbox metric in map, got %#v", listed)
	}
}

func TestSandboxMetricsValidatesRangeAndMissingSandbox(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	rangeReq := httptest.NewRequest(http.MethodGet, "/sandboxes/sbx_missing/metrics?start=20&end=10", nil)
	rangeRec := httptest.NewRecorder()
	app.ServeHTTP(rangeRec, rangeReq)
	if rangeRec.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid range status %d, got %d: %s", http.StatusBadRequest, rangeRec.Code, rangeRec.Body.String())
	}

	missingReq := httptest.NewRequest(http.MethodGet, "/sandboxes/sbx_missing/metrics", nil)
	missingRec := httptest.NewRecorder()
	app.ServeHTTP(missingRec, missingReq)
	if missingRec.Code != http.StatusNotFound {
		t.Fatalf("expected missing sandbox status %d, got %d: %s", http.StatusNotFound, missingRec.Code, missingRec.Body.String())
	}
}

func TestTeamEndpointsAndAdminKillUseLocalStore(t *testing.T) {
	cfg := DefaultConfig()
	app := newTestApp(t, cfg)

	for i := 0; i < 2; i++ {
		createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"base"}`))
		createRec := httptest.NewRecorder()
		app.ServeHTTP(createRec, createReq)
		if createRec.Code != http.StatusCreated {
			t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
		}
	}

	teamsReq := httptest.NewRequest(http.MethodGet, "/teams", nil)
	teamsRec := httptest.NewRecorder()
	app.ServeHTTP(teamsRec, teamsReq)
	if teamsRec.Code != http.StatusOK {
		t.Fatalf("expected teams status %d, got %d: %s", http.StatusOK, teamsRec.Code, teamsRec.Body.String())
	}

	var teams []e2bapi.Team
	if err := json.Unmarshal(teamsRec.Body.Bytes(), &teams); err != nil {
		t.Fatalf("decode teams response: %v", err)
	}
	if len(teams) != 1 || teams[0].TeamID != localTeamID || teams[0].ApiKey != "local" || !teams[0].IsDefault {
		t.Fatalf("unexpected teams response: %#v", teams)
	}

	metricsReq := httptest.NewRequest(http.MethodGet, "/teams/"+localTeamID+"/metrics", nil)
	metricsRec := httptest.NewRecorder()
	app.ServeHTTP(metricsRec, metricsReq)
	if metricsRec.Code != http.StatusOK {
		t.Fatalf("expected metrics status %d, got %d: %s", http.StatusOK, metricsRec.Code, metricsRec.Body.String())
	}

	var metrics []e2bapi.TeamMetric
	if err := json.Unmarshal(metricsRec.Body.Bytes(), &metrics); err != nil {
		t.Fatalf("decode team metrics response: %v", err)
	}
	if len(metrics) != 1 || metrics[0].ConcurrentSandboxes != 2 {
		t.Fatalf("unexpected team metrics response: %#v", metrics)
	}

	maxReq := httptest.NewRequest(http.MethodGet, "/teams/"+localTeamID+"/metrics/max?metric=concurrent_sandboxes", nil)
	maxRec := httptest.NewRecorder()
	app.ServeHTTP(maxRec, maxReq)
	if maxRec.Code != http.StatusOK {
		t.Fatalf("expected max metrics status %d, got %d: %s", http.StatusOK, maxRec.Code, maxRec.Body.String())
	}

	var maxMetric e2bapi.MaxTeamMetric
	if err := json.Unmarshal(maxRec.Body.Bytes(), &maxMetric); err != nil {
		t.Fatalf("decode max team metric response: %v", err)
	}
	if maxMetric.Value != 2 {
		t.Fatalf("expected max concurrent sandboxes 2, got %#v", maxMetric)
	}

	cancelReq := httptest.NewRequest(http.MethodPost, "/admin/teams/"+localTeamID+"/builds/cancel", nil)
	cancelRec := httptest.NewRecorder()
	app.ServeHTTP(cancelRec, cancelReq)
	if cancelRec.Code != http.StatusOK {
		t.Fatalf("expected cancel builds status %d, got %d: %s", http.StatusOK, cancelRec.Code, cancelRec.Body.String())
	}

	killReq := httptest.NewRequest(http.MethodPost, "/admin/teams/"+localTeamID+"/sandboxes/kill", nil)
	killRec := httptest.NewRecorder()
	app.ServeHTTP(killRec, killReq)
	if killRec.Code != http.StatusOK {
		t.Fatalf("expected admin kill status %d, got %d: %s", http.StatusOK, killRec.Code, killRec.Body.String())
	}

	var killed e2bapi.AdminSandboxKillResult
	if err := json.Unmarshal(killRec.Body.Bytes(), &killed); err != nil {
		t.Fatalf("decode admin kill response: %v", err)
	}
	if killed.KilledCount != 2 || killed.FailedCount != 0 {
		t.Fatalf("unexpected admin kill response: %#v", killed)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/v2/sandboxes", nil)
	listRec := httptest.NewRecorder()
	app.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d: %s", http.StatusOK, listRec.Code, listRec.Body.String())
	}

	var listed []e2bapi.ListedSandbox
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("expected all sandboxes killed, got %#v", listed)
	}
}

func TestSandboxSnapshotsUseRuntime(t *testing.T) {
	runtime := &recordingRuntime{}
	app, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"base"}`))
	createRec := httptest.NewRecorder()
	app.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	var created SandboxResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	snapshotReq := httptest.NewRequest(http.MethodPost, "/sandboxes/"+created.SandboxID+"/snapshots", bytes.NewBufferString(`{"name":"savepoint"}`))
	snapshotRec := httptest.NewRecorder()
	app.ServeHTTP(snapshotRec, snapshotReq)
	if snapshotRec.Code != http.StatusCreated {
		t.Fatalf("expected snapshot status %d, got %d: %s", http.StatusCreated, snapshotRec.Code, snapshotRec.Body.String())
	}

	var snapshot e2bapi.SnapshotInfo
	if err := json.Unmarshal(snapshotRec.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode snapshot response: %v", err)
	}
	if snapshot.SnapshotID != "snapshot-"+created.SandboxID {
		t.Fatalf("unexpected snapshot response: %#v", snapshot)
	}

	if len(runtime.snapshotCreateCalls) != 1 {
		t.Fatalf("expected 1 runtime snapshot call, got %d", len(runtime.snapshotCreateCalls))
	}
	if runtime.snapshotCreateCalls[0].Record.ID != created.SandboxID {
		t.Fatalf("expected runtime snapshot sandbox id %q, got %q", created.SandboxID, runtime.snapshotCreateCalls[0].Record.ID)
	}
	if runtime.snapshotCreateCalls[0].Request.Name == nil || *runtime.snapshotCreateCalls[0].Request.Name != "savepoint" {
		t.Fatalf("expected runtime snapshot name savepoint, got %#v", runtime.snapshotCreateCalls[0].Request)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/snapshots?sandboxID="+created.SandboxID+"&limit=1", nil)
	listRec := httptest.NewRecorder()
	app.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected list snapshots status %d, got %d: %s", http.StatusOK, listRec.Code, listRec.Body.String())
	}

	var snapshots []e2bapi.SnapshotInfo
	if err := json.Unmarshal(listRec.Body.Bytes(), &snapshots); err != nil {
		t.Fatalf("decode list snapshots response: %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].SnapshotID != snapshot.SnapshotID {
		t.Fatalf("unexpected list snapshots response: %#v", snapshots)
	}
	if len(runtime.snapshotListCalls) != 1 || runtime.snapshotListCalls[0].SandboxID != created.SandboxID || runtime.snapshotListCalls[0].Limit != 1 {
		t.Fatalf("unexpected runtime snapshot list calls: %#v", runtime.snapshotListCalls)
	}
}

func TestPauseSandboxAndConnectResumes(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"base"}`))
	createRec := httptest.NewRecorder()

	app.ServeHTTP(createRec, createReq)

	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	var created SandboxResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	pauseReq := httptest.NewRequest(http.MethodPost, "/sandboxes/"+created.SandboxID+"/pause", nil)
	pauseRec := httptest.NewRecorder()

	app.ServeHTTP(pauseRec, pauseReq)

	if pauseRec.Code != http.StatusNoContent {
		t.Fatalf("expected pause status %d, got %d: %s", http.StatusNoContent, pauseRec.Code, pauseRec.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/v2/sandboxes", nil)
	listRec := httptest.NewRecorder()

	app.ServeHTTP(listRec, listReq)

	var sandboxes []ListedSandboxResponse
	if err := json.Unmarshal(listRec.Body.Bytes(), &sandboxes); err != nil {
		t.Fatalf("decode list response: %v", err)
	}

	if len(sandboxes) != 1 || sandboxes[0].State != "paused" {
		t.Fatalf("expected paused sandbox in list, got %#v", sandboxes)
	}

	repeatedPauseReq := httptest.NewRequest(http.MethodPost, "/sandboxes/"+created.SandboxID+"/pause", nil)
	repeatedPauseRec := httptest.NewRecorder()

	app.ServeHTTP(repeatedPauseRec, repeatedPauseReq)

	if repeatedPauseRec.Code != http.StatusConflict {
		t.Fatalf("expected repeated pause status %d, got %d: %s", http.StatusConflict, repeatedPauseRec.Code, repeatedPauseRec.Body.String())
	}

	connectReq := httptest.NewRequest(http.MethodPost, "/sandboxes/"+created.SandboxID+"/connect", bytes.NewBufferString(`{"timeout":300}`))
	connectRec := httptest.NewRecorder()

	app.ServeHTTP(connectRec, connectReq)

	if connectRec.Code != http.StatusCreated {
		t.Fatalf("expected connect status %d, got %d: %s", http.StatusCreated, connectRec.Code, connectRec.Body.String())
	}

	listAfterConnectReq := httptest.NewRequest(http.MethodGet, "/v2/sandboxes", nil)
	listAfterConnectRec := httptest.NewRecorder()

	app.ServeHTTP(listAfterConnectRec, listAfterConnectReq)

	var afterConnect []ListedSandboxResponse
	if err := json.Unmarshal(listAfterConnectRec.Body.Bytes(), &afterConnect); err != nil {
		t.Fatalf("decode list after connect response: %v", err)
	}

	if len(afterConnect) != 1 || afterConnect[0].State != "running" {
		t.Fatalf("expected running sandbox after connect, got %#v", afterConnect)
	}
}

func TestSandboxListFiltersMetadataStateAndLimit(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	createSandbox := func(body string) string {
		req := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
		}

		var created SandboxResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode create response: %v", err)
		}
		return created.SandboxID
	}

	pausedID := createSandbox(`{"templateID":"base","metadata":{"source":"filter","kind":"paused"}}`)
	runningID := createSandbox(`{"templateID":"base","metadata":{"source":"filter","kind":"running"}}`)
	_ = createSandbox(`{"templateID":"base","metadata":{"source":"other"}}`)

	pauseReq := httptest.NewRequest(http.MethodPost, "/sandboxes/"+pausedID+"/pause", nil)
	pauseRec := httptest.NewRecorder()
	app.ServeHTTP(pauseRec, pauseReq)
	if pauseRec.Code != http.StatusNoContent {
		t.Fatalf("expected pause status %d, got %d: %s", http.StatusNoContent, pauseRec.Code, pauseRec.Body.String())
	}

	v1Req := httptest.NewRequest(http.MethodGet, "/sandboxes?metadata=source%3Dfilter", nil)
	v1Rec := httptest.NewRecorder()
	app.ServeHTTP(v1Rec, v1Req)
	if v1Rec.Code != http.StatusOK {
		t.Fatalf("expected v1 list status %d, got %d: %s", http.StatusOK, v1Rec.Code, v1Rec.Body.String())
	}
	var v1Items []ListedSandboxResponse
	if err := json.Unmarshal(v1Rec.Body.Bytes(), &v1Items); err != nil {
		t.Fatalf("decode v1 list: %v", err)
	}
	if len(v1Items) != 1 || v1Items[0].SandboxID != runningID {
		t.Fatalf("expected v1 metadata list to include only running sandbox %s, got %#v", runningID, v1Items)
	}

	v2Req := httptest.NewRequest(http.MethodGet, "/v2/sandboxes?metadata=source%3Dfilter&state=paused&limit=1", nil)
	v2Rec := httptest.NewRecorder()
	app.ServeHTTP(v2Rec, v2Req)
	if v2Rec.Code != http.StatusOK {
		t.Fatalf("expected v2 list status %d, got %d: %s", http.StatusOK, v2Rec.Code, v2Rec.Body.String())
	}
	if got := v2Rec.Header().Get("X-Total-Running"); got != "1" {
		t.Fatalf("expected X-Total-Running 1, got %q", got)
	}
	var v2Items []ListedSandboxResponse
	if err := json.Unmarshal(v2Rec.Body.Bytes(), &v2Items); err != nil {
		t.Fatalf("decode v2 list: %v", err)
	}
	if len(v2Items) != 1 || v2Items[0].SandboxID != pausedID || v2Items[0].State != "paused" {
		t.Fatalf("expected v2 filtered paused sandbox %s, got %#v", pausedID, v2Items)
	}
}

func TestConnectMissingSandbox(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	req := httptest.NewRequest(http.MethodPost, "/sandboxes/sbx_missing/connect", bytes.NewBufferString(`{"timeout":300}`))
	rec := httptest.NewRecorder()

	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNotFound, rec.Code, rec.Body.String())
	}
}

func TestConnectRequiresTimeout(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	req := httptest.NewRequest(http.MethodPost, "/sandboxes/sbx_missing/connect", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected missing connect timeout status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
}

func TestCreateSandboxRequiresTemplateID(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	req := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()

	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
	}
}

func TestDockerRuntimePassesTemplateIDToRuntime(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Runtime.Type = "docker"
	runtime := &recordingRuntime{}
	app, err := NewAppWithRuntime(cfg, log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"ubuntu"}`))
	rec := httptest.NewRecorder()

	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	if len(runtime.createCalls) != 1 || runtime.createCalls[0].TemplateID != "ubuntu" {
		t.Fatalf("expected runtime create with ubuntu, got %#v", runtime.createCalls)
	}
}

func TestCreateSandboxPassesVolumeMountsToRuntime(t *testing.T) {
	runtime := &recordingRuntime{}
	app, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"base","volumeMounts":[{"name":"data","path":"/mnt/data"}]}`))
	rec := httptest.NewRecorder()

	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	if len(runtime.createCalls) != 1 {
		t.Fatalf("expected one runtime create call, got %d", len(runtime.createCalls))
	}
	if got := runtime.createCalls[0].VolumeMounts; len(got) != 1 || got[0].Name != "data" || got[0].Path != "/mnt/data" {
		t.Fatalf("expected volume mount passed to runtime, got %#v", got)
	}

	var sandbox SandboxResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &sandbox); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if len(sandbox.VolumeMounts) != 1 || sandbox.VolumeMounts[0].Name != "data" || sandbox.VolumeMounts[0].Path != "/mnt/data" {
		t.Fatalf("expected volume mount in create response, got %#v", sandbox.VolumeMounts)
	}
}

func TestListTemplatesUsesRuntime(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Runtime.Type = "docker"
	now := time.Now().UTC()
	runtime := &recordingRuntime{
		templates: []SandboxRuntimeTemplate{
			{
				TemplateID:  "runtime",
				Names:       []string{"runtime"},
				ImageRef:    "registry.example.com/team/runtime:latest",
				BuildCount:  1,
				BuildID:     "sha",
				BuildStatus: "ready",
				CPUCount:    1,
				MemoryMB:    512,
				CreatedAt:   now,
				UpdatedAt:   now,
			},
		},
	}
	app, err := NewAppWithRuntime(cfg, log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/templates", nil)
	rec := httptest.NewRecorder()

	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var templates []TemplateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &templates); err != nil {
		t.Fatalf("decode templates response: %v", err)
	}

	if len(templates) != 1 || templates[0].TemplateID != "runtime" {
		t.Fatalf("expected runtime template, got %#v", templates)
	}

	if templates[0].ImageRef != "registry.example.com/team/runtime:latest" {
		t.Fatalf("expected full image ref, got %q", templates[0].ImageRef)
	}

	if templates[0].EnvdVersion != defaultEnvdVersion {
		t.Fatalf("expected envd version %q, got %q", defaultEnvdVersion, templates[0].EnvdVersion)
	}
}

func TestTemplateReadEndpointsUseRuntime(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Runtime.Type = "docker"
	now := time.Now().UTC()
	runtime := &recordingRuntime{
		templates: []SandboxRuntimeTemplate{
			{
				TemplateID:  "runtime",
				Names:       []string{"runtime", "runtime:latest"},
				ImageRef:    "registry.example.com/team/runtime:latest",
				BuildCount:  1,
				BuildID:     "sha",
				BuildStatus: "ready",
				CPUCount:    2,
				DiskSizeMB:  1024,
				MemoryMB:    2048,
				Public:      true,
				SpawnCount:  3,
				CreatedAt:   now,
				UpdatedAt:   now.Add(time.Minute),
			},
		},
	}
	app, err := NewAppWithRuntime(cfg, log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	detailReq := httptest.NewRequest(http.MethodGet, "/templates/runtime?limit=1", nil)
	detailRec := httptest.NewRecorder()
	app.ServeHTTP(detailRec, detailReq)
	if detailRec.Code != http.StatusOK {
		t.Fatalf("expected detail status %d, got %d: %s", http.StatusOK, detailRec.Code, detailRec.Body.String())
	}

	var detail e2bapi.TemplateWithBuilds
	if err := json.Unmarshal(detailRec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode template detail response: %v", err)
	}
	if detail.TemplateID != "runtime" || len(detail.Builds) != 1 || detail.Builds[0].CpuCount != 2 {
		t.Fatalf("unexpected template detail: %#v", detail)
	}
	if detail.Builds[0].BuildID.String() == "" {
		t.Fatalf("expected stable build uuid, got %#v", detail.Builds[0].BuildID)
	}

	aliasReq := httptest.NewRequest(http.MethodGet, "/templates/aliases/runtime:latest", nil)
	aliasRec := httptest.NewRecorder()
	app.ServeHTTP(aliasRec, aliasReq)
	if aliasRec.Code != http.StatusOK {
		t.Fatalf("expected alias status %d, got %d: %s", http.StatusOK, aliasRec.Code, aliasRec.Body.String())
	}

	var alias e2bapi.TemplateAliasResponse
	if err := json.Unmarshal(aliasRec.Body.Bytes(), &alias); err != nil {
		t.Fatalf("decode template alias response: %v", err)
	}
	if alias.TemplateID != "runtime" || !alias.Public {
		t.Fatalf("unexpected template alias response: %#v", alias)
	}

	tagsReq := httptest.NewRequest(http.MethodGet, "/templates/runtime/tags", nil)
	tagsRec := httptest.NewRecorder()
	app.ServeHTTP(tagsRec, tagsReq)
	if tagsRec.Code != http.StatusOK {
		t.Fatalf("expected tags status %d, got %d: %s", http.StatusOK, tagsRec.Code, tagsRec.Body.String())
	}

	var tags []e2bapi.TemplateTag
	if err := json.Unmarshal(tagsRec.Body.Bytes(), &tags); err != nil {
		t.Fatalf("decode template tags response: %v", err)
	}
	if len(tags) != 1 || tags[0].Tag != "latest" || tags[0].BuildID != detail.Builds[0].BuildID {
		t.Fatalf("unexpected template tags response: %#v", tags)
	}
}

func TestLocalTemplateManagementEndpoints(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	createReq := httptest.NewRequest(http.MethodPost, "/v3/templates", bytes.NewBufferString(`{"name":"managed-template:v1","tags":["prod"],"cpuCount":2,"memoryMB":1024}`))
	createRec := httptest.NewRecorder()
	app.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusAccepted {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusAccepted, createRec.Code, createRec.Body.String())
	}

	var created e2bapi.TemplateRequestResponseV3
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created template: %v", err)
	}
	if created.TemplateID != "managed-template" || created.BuildID == "" {
		t.Fatalf("unexpected created template: %#v", created)
	}
	if !reflect.DeepEqual(created.Tags, []string{"prod", "v1"}) {
		t.Fatalf("unexpected created tags: %#v", created.Tags)
	}

	tagsReq := httptest.NewRequest(http.MethodGet, "/templates/managed-template/tags", nil)
	tagsRec := httptest.NewRecorder()
	app.ServeHTTP(tagsRec, tagsReq)
	if tagsRec.Code != http.StatusOK {
		t.Fatalf("expected tags status %d, got %d: %s", http.StatusOK, tagsRec.Code, tagsRec.Body.String())
	}

	var tags []e2bapi.TemplateTag
	if err := json.Unmarshal(tagsRec.Body.Bytes(), &tags); err != nil {
		t.Fatalf("decode tags: %v", err)
	}
	if len(tags) != 2 || tags[0].Tag != "prod" || tags[1].Tag != "v1" {
		t.Fatalf("unexpected tags: %#v", tags)
	}
	if !templateTagsContainBuildID(tags, created.BuildID) {
		t.Fatalf("expected template tags to preserve created buildID %q, got %#v", created.BuildID, tags)
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/templates/managed-template/builds/"+created.BuildID+"/status?limit=1", nil)
	statusRec := httptest.NewRecorder()
	app.ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("expected status response %d, got %d: %s", http.StatusOK, statusRec.Code, statusRec.Body.String())
	}

	var buildInfo e2bapi.TemplateBuildInfo
	if err := json.Unmarshal(statusRec.Body.Bytes(), &buildInfo); err != nil {
		t.Fatalf("decode build info: %v", err)
	}
	if buildInfo.TemplateID != "managed-template" || buildInfo.Status != e2bapi.TemplateBuildStatusReady || len(buildInfo.LogEntries) != 1 {
		t.Fatalf("unexpected build info: %#v", buildInfo)
	}

	initialDetailReq := httptest.NewRequest(http.MethodGet, "/templates/managed-template?limit=10", nil)
	initialDetailRec := httptest.NewRecorder()
	app.ServeHTTP(initialDetailRec, initialDetailReq)
	if initialDetailRec.Code != http.StatusOK {
		t.Fatalf("expected initial detail status %d, got %d: %s", http.StatusOK, initialDetailRec.Code, initialDetailRec.Body.String())
	}
	var initialDetail e2bapi.TemplateWithBuilds
	if err := json.Unmarshal(initialDetailRec.Body.Bytes(), &initialDetail); err != nil {
		t.Fatalf("decode initial detail: %v", err)
	}
	if !templateBuildListContains(initialDetail.Builds, created.BuildID) {
		t.Fatalf("expected template detail to preserve created buildID %q, got %#v", created.BuildID, initialDetail.Builds)
	}

	firstBuildReq := httptest.NewRequest(http.MethodPost, "/templates/managed-template/builds/build-one", nil)
	firstBuildRec := httptest.NewRecorder()
	app.ServeHTTP(firstBuildRec, firstBuildReq)
	if firstBuildRec.Code != http.StatusAccepted {
		t.Fatalf("expected first build status %d, got %d: %s", http.StatusAccepted, firstBuildRec.Code, firstBuildRec.Body.String())
	}

	secondBuildReq := httptest.NewRequest(http.MethodPost, "/templates/managed-template/builds/build-two", nil)
	secondBuildRec := httptest.NewRecorder()
	app.ServeHTTP(secondBuildRec, secondBuildReq)
	if secondBuildRec.Code != http.StatusAccepted {
		t.Fatalf("expected second build status %d, got %d: %s", http.StatusAccepted, secondBuildRec.Code, secondBuildRec.Body.String())
	}

	oldStatusReq := httptest.NewRequest(http.MethodGet, "/templates/managed-template/builds/build-one/status", nil)
	oldStatusRec := httptest.NewRecorder()
	app.ServeHTTP(oldStatusRec, oldStatusReq)
	if oldStatusRec.Code != http.StatusOK {
		t.Fatalf("expected old build status response %d, got %d: %s", http.StatusOK, oldStatusRec.Code, oldStatusRec.Body.String())
	}

	detailReq := httptest.NewRequest(http.MethodGet, "/templates/managed-template?limit=10", nil)
	detailRec := httptest.NewRecorder()
	app.ServeHTTP(detailRec, detailReq)
	if detailRec.Code != http.StatusOK {
		t.Fatalf("expected detail status %d, got %d: %s", http.StatusOK, detailRec.Code, detailRec.Body.String())
	}
	var detail e2bapi.TemplateWithBuilds
	if err := json.Unmarshal(detailRec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail with builds: %v", err)
	}
	if !templateBuildListContains(detail.Builds, stableTemplateBuildUUID("managed-template", "build-one").String()) ||
		!templateBuildListContains(detail.Builds, stableTemplateBuildUUID("managed-template", "build-two").String()) {
		t.Fatalf("expected detail to include both local builds, got %#v", detail.Builds)
	}

	patchReq := httptest.NewRequest(http.MethodPatch, "/v2/templates/managed-template", bytes.NewBufferString(`{"public":true}`))
	patchRec := httptest.NewRecorder()
	app.ServeHTTP(patchRec, patchReq)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("expected patch status %d, got %d: %s", http.StatusOK, patchRec.Code, patchRec.Body.String())
	}

	deleteTagsReq := httptest.NewRequest(http.MethodDelete, "/templates/tags", bytes.NewBufferString(`{"name":"managed-template","tags":["prod"]}`))
	deleteTagsRec := httptest.NewRecorder()
	app.ServeHTTP(deleteTagsRec, deleteTagsReq)
	if deleteTagsRec.Code != http.StatusNoContent {
		t.Fatalf("expected delete tags status %d, got %d: %s", http.StatusNoContent, deleteTagsRec.Code, deleteTagsRec.Body.String())
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/templates/managed-template", nil)
	deleteRec := httptest.NewRecorder()
	app.ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("expected delete status %d, got %d: %s", http.StatusNoContent, deleteRec.Code, deleteRec.Body.String())
	}
}

func TestTemplateCreateDelegatesToRuntimeBuilder(t *testing.T) {
	runtime := &recordingTemplateBuilderRuntime{}
	app, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/templates", bytes.NewBufferString(`{"alias":"docker-template","dockerfile":"FROM alpine:3.20","cpuCount":2,"memoryMB":1024}`))
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusAccepted, rec.Code, rec.Body.String())
	}
	if len(runtime.buildRequests) != 1 || runtime.buildRequests[0].Dockerfile != "FROM alpine:3.20" {
		t.Fatalf("expected runtime builder call, got %#v", runtime.buildRequests)
	}

	var created e2bapi.TemplateLegacy
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created template: %v", err)
	}
	if created.TemplateID != "docker-template" || created.BuildID != "runtime-build" || created.DiskSizeMB != 64 {
		t.Fatalf("unexpected created template: %#v", created)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/templates", nil)
	listRec := httptest.NewRecorder()
	app.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d: %s", http.StatusOK, listRec.Code, listRec.Body.String())
	}

	var templates []TemplateResponse
	if err := json.Unmarshal(listRec.Body.Bytes(), &templates); err != nil {
		t.Fatalf("decode templates: %v", err)
	}
	var builtTemplate *TemplateResponse
	for i := range templates {
		if templates[i].TemplateID == "docker-template" {
			builtTemplate = &templates[i]
			break
		}
	}
	if builtTemplate == nil || builtTemplate.ImageRef != "registry.example.test/docker-template:latest" {
		t.Fatalf("expected registered built template, got %#v", templates)
	}
}

func TestTemplateBuildStartV2DelegatesToRuntimeBuilder(t *testing.T) {
	runtime := &recordingTemplateBuilderRuntime{startBuildReturned: make(chan struct{})}
	app, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/v3/templates", bytes.NewBufferString(`{"name":"sdk-template:v1","cpuCount":2,"memoryMB":1024}`))
	createRec := httptest.NewRecorder()
	app.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusAccepted {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusAccepted, createRec.Code, createRec.Body.String())
	}

	var created e2bapi.TemplateRequestResponseV3
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created template: %v", err)
	}

	startReq := httptest.NewRequest(
		http.MethodPost,
		"/v2/templates/sdk-template/builds/"+created.BuildID,
		bytes.NewBufferString(`{"fromImage":"ubuntu:22.04","steps":[{"type":"RUN","args":["echo ok"]}],"startCmd":"python main.py"}`),
	)
	startRec := httptest.NewRecorder()
	app.ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusAccepted {
		t.Fatalf("expected start status %d, got %d: %s", http.StatusAccepted, startRec.Code, startRec.Body.String())
	}
	waitForSignal(t, runtime.startBuildReturned, "runtime start build")
	if len(runtime.startBuildRequests) != 1 {
		t.Fatalf("expected runtime start build call, got %#v", runtime.startBuildRequests)
	}
	if runtime.startBuildRequests[0].FromImage == nil || *runtime.startBuildRequests[0].FromImage != "ubuntu:22.04" {
		t.Fatalf("expected fromImage to be forwarded, got %#v", runtime.startBuildRequests[0])
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/templates/sdk-template/builds/"+created.BuildID+"/status", nil)
	statusRec := httptest.NewRecorder()
	app.ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("expected status response %d, got %d: %s", http.StatusOK, statusRec.Code, statusRec.Body.String())
	}

	var buildInfo e2bapi.TemplateBuildInfo
	if err := json.Unmarshal(statusRec.Body.Bytes(), &buildInfo); err != nil {
		t.Fatalf("decode build info: %v", err)
	}
	if buildInfo.Status != e2bapi.TemplateBuildStatusReady || !buildLogMessagesContain(buildInfo.LogEntries, "runtime v2 builder completed") {
		t.Fatalf("unexpected build info: %#v", buildInfo)
	}
}

func TestTemplateBuildStartV2RunsAsynchronously(t *testing.T) {
	runtime := &recordingTemplateBuilderRuntime{
		startBuildEntered:  make(chan struct{}),
		releaseStartBuild:  make(chan struct{}),
		startBuildReturned: make(chan struct{}),
	}
	app, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/v3/templates", bytes.NewBufferString(`{"name":"async-template"}`))
	createRec := httptest.NewRecorder()
	app.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusAccepted {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusAccepted, createRec.Code, createRec.Body.String())
	}

	var created e2bapi.TemplateRequestResponseV3
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created template: %v", err)
	}

	startReq := httptest.NewRequest(
		http.MethodPost,
		"/v2/templates/async-template/builds/"+created.BuildID,
		bytes.NewBufferString(`{"fromImage":"ubuntu:22.04","steps":[{"type":"RUN","args":["echo ok"]}]}`),
	)
	startRec := httptest.NewRecorder()
	app.ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusAccepted {
		t.Fatalf("expected start status %d, got %d: %s", http.StatusAccepted, startRec.Code, startRec.Body.String())
	}

	waitForSignal(t, runtime.startBuildEntered, "runtime start build entered")

	statusReq := httptest.NewRequest(http.MethodGet, "/templates/async-template/builds/"+created.BuildID+"/status", nil)
	statusRec := httptest.NewRecorder()
	app.ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("expected status response %d, got %d: %s", http.StatusOK, statusRec.Code, statusRec.Body.String())
	}

	var buildingInfo e2bapi.TemplateBuildInfo
	if err := json.Unmarshal(statusRec.Body.Bytes(), &buildingInfo); err != nil {
		t.Fatalf("decode building info: %v", err)
	}
	if buildingInfo.Status != e2bapi.TemplateBuildStatusBuilding {
		t.Fatalf("expected building status while runtime is blocked, got %#v", buildingInfo)
	}

	close(runtime.releaseStartBuild)
	waitForSignal(t, runtime.startBuildReturned, "runtime start build returned")

	readyReq := httptest.NewRequest(http.MethodGet, "/templates/async-template/builds/"+created.BuildID+"/status", nil)
	readyRec := httptest.NewRecorder()
	app.ServeHTTP(readyRec, readyReq)
	if readyRec.Code != http.StatusOK {
		t.Fatalf("expected ready status response %d, got %d: %s", http.StatusOK, readyRec.Code, readyRec.Body.String())
	}

	var readyInfo e2bapi.TemplateBuildInfo
	if err := json.Unmarshal(readyRec.Body.Bytes(), &readyInfo); err != nil {
		t.Fatalf("decode ready info: %v", err)
	}
	if readyInfo.Status != e2bapi.TemplateBuildStatusReady {
		t.Fatalf("expected ready status after runtime completes, got %#v", readyInfo)
	}
}

func TestCancelTeamBuildsCancelsRunningTemplateBuild(t *testing.T) {
	runtime := &recordingTemplateBuilderRuntime{
		startBuildEntered:  make(chan struct{}),
		releaseStartBuild:  make(chan struct{}),
		startBuildReturned: make(chan struct{}),
	}
	app, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/v3/templates", bytes.NewBufferString(`{"name":"cancel-template"}`))
	createRec := httptest.NewRecorder()
	app.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusAccepted {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusAccepted, createRec.Code, createRec.Body.String())
	}

	var created e2bapi.TemplateRequestResponseV3
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created template: %v", err)
	}

	startReq := httptest.NewRequest(
		http.MethodPost,
		"/v2/templates/cancel-template/builds/"+created.BuildID,
		bytes.NewBufferString(`{"fromImage":"ubuntu:22.04","steps":[{"type":"RUN","args":["sleep 30"]}]}`),
	)
	startRec := httptest.NewRecorder()
	app.ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusAccepted {
		t.Fatalf("expected start status %d, got %d: %s", http.StatusAccepted, startRec.Code, startRec.Body.String())
	}
	waitForSignal(t, runtime.startBuildEntered, "runtime start build entered")

	cancelReq := httptest.NewRequest(http.MethodPost, "/admin/teams/"+localTeamID+"/builds/cancel", nil)
	cancelRec := httptest.NewRecorder()
	app.ServeHTTP(cancelRec, cancelReq)
	if cancelRec.Code != http.StatusOK {
		t.Fatalf("expected cancel builds status %d, got %d: %s", http.StatusOK, cancelRec.Code, cancelRec.Body.String())
	}
	var cancelResult e2bapi.AdminBuildCancelResult
	if err := json.Unmarshal(cancelRec.Body.Bytes(), &cancelResult); err != nil {
		t.Fatalf("decode cancel response: %v", err)
	}
	if cancelResult.CancelledCount != 1 || cancelResult.FailedCount != 0 {
		t.Fatalf("unexpected cancel result: %#v", cancelResult)
	}
	waitForSignal(t, runtime.startBuildReturned, "runtime start build returned")

	statusReq := httptest.NewRequest(http.MethodGet, "/templates/cancel-template/builds/"+created.BuildID+"/status", nil)
	statusRec := httptest.NewRecorder()
	app.ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("expected status response %d, got %d: %s", http.StatusOK, statusRec.Code, statusRec.Body.String())
	}

	var buildInfo e2bapi.TemplateBuildInfo
	if err := json.Unmarshal(statusRec.Body.Bytes(), &buildInfo); err != nil {
		t.Fatalf("decode build info: %v", err)
	}
	if buildInfo.Status != e2bapi.TemplateBuildStatusError {
		t.Fatalf("expected cancelled build to be marked error, got %#v", buildInfo)
	}
	if !buildLogMessagesContain(buildInfo.LogEntries, "v2 template build cancelled by e2b-local") {
		t.Fatalf("expected cancellation log entry, got %#v", buildInfo.LogEntries)
	}
}

func TestTemplateBuildStartV2EnforcesConcurrencyLimit(t *testing.T) {
	runtime := &recordingTemplateBuilderRuntime{
		startBuildEntered:  make(chan struct{}),
		releaseStartBuild:  make(chan struct{}),
		startBuildReturned: make(chan struct{}),
	}
	cfg := DefaultConfig()
	cfg.TemplateBuilds.MaxConcurrent = 1
	app, err := NewAppWithRuntime(cfg, log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	createBusyReq := httptest.NewRequest(http.MethodPost, "/v3/templates", bytes.NewBufferString(`{"name":"busy-template"}`))
	createBusyRec := httptest.NewRecorder()
	app.ServeHTTP(createBusyRec, createBusyReq)
	if createBusyRec.Code != http.StatusAccepted {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusAccepted, createBusyRec.Code, createBusyRec.Body.String())
	}
	var busy e2bapi.TemplateRequestResponseV3
	if err := json.Unmarshal(createBusyRec.Body.Bytes(), &busy); err != nil {
		t.Fatalf("decode busy template: %v", err)
	}

	startBusyReq := httptest.NewRequest(
		http.MethodPost,
		"/v2/templates/busy-template/builds/"+busy.BuildID,
		bytes.NewBufferString(`{"fromImage":"ubuntu:22.04","steps":[{"type":"RUN","args":["sleep 30"]}]}`),
	)
	startBusyRec := httptest.NewRecorder()
	app.ServeHTTP(startBusyRec, startBusyReq)
	if startBusyRec.Code != http.StatusAccepted {
		t.Fatalf("expected first build status %d, got %d: %s", http.StatusAccepted, startBusyRec.Code, startBusyRec.Body.String())
	}
	waitForSignal(t, runtime.startBuildEntered, "runtime start build entered")

	createBlockedReq := httptest.NewRequest(http.MethodPost, "/v3/templates", bytes.NewBufferString(`{"name":"blocked-template"}`))
	createBlockedRec := httptest.NewRecorder()
	app.ServeHTTP(createBlockedRec, createBlockedReq)
	if createBlockedRec.Code != http.StatusAccepted {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusAccepted, createBlockedRec.Code, createBlockedRec.Body.String())
	}
	var blocked e2bapi.TemplateRequestResponseV3
	if err := json.Unmarshal(createBlockedRec.Body.Bytes(), &blocked); err != nil {
		t.Fatalf("decode blocked template: %v", err)
	}

	startBlockedReq := httptest.NewRequest(
		http.MethodPost,
		"/v2/templates/blocked-template/builds/"+blocked.BuildID,
		bytes.NewBufferString(`{"fromImage":"ubuntu:22.04","steps":[{"type":"RUN","args":["echo ok"]}]}`),
	)
	startBlockedRec := httptest.NewRecorder()
	app.ServeHTTP(startBlockedRec, startBlockedReq)
	if startBlockedRec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected second build status %d, got %d: %s", http.StatusTooManyRequests, startBlockedRec.Code, startBlockedRec.Body.String())
	}

	close(runtime.releaseStartBuild)
	waitForSignal(t, runtime.startBuildReturned, "runtime start build returned")
	if len(runtime.startBuildRequests) != 1 {
		t.Fatalf("expected only one runtime build call, got %#v", runtime.startBuildRequests)
	}
}

func TestAppShutdownCancelsRunningTemplateBuild(t *testing.T) {
	runtime := &recordingTemplateBuilderRuntime{
		startBuildEntered:  make(chan struct{}),
		releaseStartBuild:  make(chan struct{}),
		startBuildReturned: make(chan struct{}),
	}
	app, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	shutdowner, ok := app.(interface {
		Shutdown(context.Context) error
	})
	if !ok {
		t.Fatal("expected app to support shutdown")
	}

	createReq := httptest.NewRequest(http.MethodPost, "/v3/templates", bytes.NewBufferString(`{"name":"shutdown-template"}`))
	createRec := httptest.NewRecorder()
	app.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusAccepted {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusAccepted, createRec.Code, createRec.Body.String())
	}
	var created e2bapi.TemplateRequestResponseV3
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created template: %v", err)
	}

	startReq := httptest.NewRequest(
		http.MethodPost,
		"/v2/templates/shutdown-template/builds/"+created.BuildID,
		bytes.NewBufferString(`{"fromImage":"ubuntu:22.04","steps":[{"type":"RUN","args":["sleep 30"]}]}`),
	)
	startRec := httptest.NewRecorder()
	app.ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusAccepted {
		t.Fatalf("expected start status %d, got %d: %s", http.StatusAccepted, startRec.Code, startRec.Body.String())
	}
	waitForSignal(t, runtime.startBuildEntered, "runtime start build entered")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := shutdowner.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown app: %v", err)
	}
	waitForSignal(t, runtime.startBuildReturned, "runtime start build returned")

	statusReq := httptest.NewRequest(http.MethodGet, "/templates/shutdown-template/builds/"+created.BuildID+"/status", nil)
	statusRec := httptest.NewRecorder()
	app.ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("expected status response %d, got %d: %s", http.StatusOK, statusRec.Code, statusRec.Body.String())
	}
	var buildInfo e2bapi.TemplateBuildInfo
	if err := json.Unmarshal(statusRec.Body.Bytes(), &buildInfo); err != nil {
		t.Fatalf("decode build info: %v", err)
	}
	if buildInfo.Status != e2bapi.TemplateBuildStatusError {
		t.Fatalf("expected shutdown build to be marked error, got %#v", buildInfo)
	}
}

func TestTemplateCopyUploadIsPassedToRuntimeBuilder(t *testing.T) {
	runtime := &recordingTemplateBuilderRuntime{startBuildReturned: make(chan struct{})}
	app, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/v3/templates", bytes.NewBufferString(`{"name":"copy-template"}`))
	createRec := httptest.NewRecorder()
	app.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusAccepted {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusAccepted, createRec.Code, createRec.Body.String())
	}

	var created e2bapi.TemplateRequestResponseV3
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created template: %v", err)
	}

	hash := "hash-copy"
	linkReq := httptest.NewRequest(http.MethodGet, "/templates/copy-template/files/"+hash, nil)
	linkRec := httptest.NewRecorder()
	app.ServeHTTP(linkRec, linkReq)
	if linkRec.Code != http.StatusCreated {
		t.Fatalf("expected link status %d, got %d: %s", http.StatusCreated, linkRec.Code, linkRec.Body.String())
	}

	var link e2bapi.TemplateBuildFileUpload
	if err := json.Unmarshal(linkRec.Body.Bytes(), &link); err != nil {
		t.Fatalf("decode upload link: %v", err)
	}
	if link.Present || link.Url == nil || *link.Url == "" {
		t.Fatalf("expected upload URL and missing cache, got %#v", link)
	}

	archive := gzipTarBytes(t, map[string]string{"src/file.txt": "hello"})
	uploadReq := httptest.NewRequest(http.MethodPut, *link.Url, bytes.NewReader(archive))
	uploadRec := httptest.NewRecorder()
	app.ServeHTTP(uploadRec, uploadReq)
	if uploadRec.Code != http.StatusOK {
		t.Fatalf("expected upload status %d, got %d: %s", http.StatusOK, uploadRec.Code, uploadRec.Body.String())
	}

	secondLinkReq := httptest.NewRequest(http.MethodGet, "/templates/copy-template/files/"+hash, nil)
	secondLinkRec := httptest.NewRecorder()
	app.ServeHTTP(secondLinkRec, secondLinkReq)
	if secondLinkRec.Code != http.StatusCreated {
		t.Fatalf("expected second link status %d, got %d: %s", http.StatusCreated, secondLinkRec.Code, secondLinkRec.Body.String())
	}
	var secondLink e2bapi.TemplateBuildFileUpload
	if err := json.Unmarshal(secondLinkRec.Body.Bytes(), &secondLink); err != nil {
		t.Fatalf("decode second upload link: %v", err)
	}
	if !secondLink.Present {
		t.Fatalf("expected uploaded cache to be present, got %#v", secondLink)
	}

	startReq := httptest.NewRequest(
		http.MethodPost,
		"/v2/templates/copy-template/builds/"+created.BuildID,
		bytes.NewBufferString(`{"fromImage":"ubuntu:22.04","steps":[{"type":"COPY","args":["src/file.txt","/app/file.txt"],"filesHash":"`+hash+`"}]}`),
	)
	startRec := httptest.NewRecorder()
	app.ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusAccepted {
		t.Fatalf("expected start status %d, got %d: %s", http.StatusAccepted, startRec.Code, startRec.Body.String())
	}
	waitForSignal(t, runtime.startBuildReturned, "runtime start build")
	if len(runtime.startBuildFiles) != 1 || len(runtime.startBuildFiles[0]) != 1 {
		t.Fatalf("expected runtime builder to receive uploaded file, got %#v", runtime.startBuildFiles)
	}
	forwardedFile := runtime.startBuildFiles[0][0]
	forwardedData, err := forwardedFile.ReadAll()
	if err != nil {
		t.Fatalf("read uploaded file forwarded to builder: %v", err)
	}
	if forwardedFile.Hash != hash || !bytes.Equal(forwardedData, archive) {
		t.Fatalf("unexpected uploaded file forwarded to builder: %#v", forwardedFile)
	}
}

func TestLocalCredentialManagementEndpointsAreNotImplemented(t *testing.T) {
	app := newTestApp(t, DefaultConfig())
	id := "00000000-0000-0000-0000-000000000000"

	tokenReq := httptest.NewRequest(http.MethodPost, "/access-tokens", bytes.NewBufferString(`{"name":"local-token"}`))
	tokenRec := httptest.NewRecorder()
	app.ServeHTTP(tokenRec, tokenReq)
	if tokenRec.Code != http.StatusNotImplemented {
		t.Fatalf("expected token status %d, got %d: %s", http.StatusNotImplemented, tokenRec.Code, tokenRec.Body.String())
	}

	deleteTokenReq := httptest.NewRequest(http.MethodDelete, "/access-tokens/"+id, nil)
	deleteTokenRec := httptest.NewRecorder()
	app.ServeHTTP(deleteTokenRec, deleteTokenReq)
	if deleteTokenRec.Code != http.StatusNotImplemented {
		t.Fatalf("expected delete token status %d, got %d: %s", http.StatusNotImplemented, deleteTokenRec.Code, deleteTokenRec.Body.String())
	}

	keyReq := httptest.NewRequest(http.MethodPost, "/api-keys", bytes.NewBufferString(`{"name":"local-key"}`))
	keyRec := httptest.NewRecorder()
	app.ServeHTTP(keyRec, keyReq)
	if keyRec.Code != http.StatusNotImplemented {
		t.Fatalf("expected key status %d, got %d: %s", http.StatusNotImplemented, keyRec.Code, keyRec.Body.String())
	}

	listKeyReq := httptest.NewRequest(http.MethodGet, "/api-keys", nil)
	listKeyRec := httptest.NewRecorder()
	app.ServeHTTP(listKeyRec, listKeyReq)
	if listKeyRec.Code != http.StatusNotImplemented {
		t.Fatalf("expected list key status %d, got %d: %s", http.StatusNotImplemented, listKeyRec.Code, listKeyRec.Body.String())
	}

	patchKeyReq := httptest.NewRequest(http.MethodPatch, "/api-keys/"+id, bytes.NewBufferString(`{"name":"renamed-key"}`))
	patchKeyRec := httptest.NewRecorder()
	app.ServeHTTP(patchKeyRec, patchKeyReq)
	if patchKeyRec.Code != http.StatusNotImplemented {
		t.Fatalf("expected patch key status %d, got %d: %s", http.StatusNotImplemented, patchKeyRec.Code, patchKeyRec.Body.String())
	}

	deleteKeyReq := httptest.NewRequest(http.MethodDelete, "/api-keys/"+id, nil)
	deleteKeyRec := httptest.NewRecorder()
	app.ServeHTTP(deleteKeyRec, deleteKeyReq)
	if deleteKeyRec.Code != http.StatusNotImplemented {
		t.Fatalf("expected delete key status %d, got %d: %s", http.StatusNotImplemented, deleteKeyRec.Code, deleteKeyRec.Body.String())
	}
}

func TestVolumeEndpointsUseRuntime(t *testing.T) {
	runtime := &recordingRuntime{}
	app, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/volumes", bytes.NewBufferString(`{"name":"test-volume"}`))
	createRec := httptest.NewRecorder()

	app.ServeHTTP(createRec, createReq)

	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	var created VolumeResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.VolumeID != "test-volume" || created.Name != "test-volume" || created.Token != "compat-volume-token-test-volume" {
		t.Fatalf("unexpected created volume response: %#v", created)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/volumes", nil)
	listRec := httptest.NewRecorder()

	app.ServeHTTP(listRec, listReq)

	if listRec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d: %s", http.StatusOK, listRec.Code, listRec.Body.String())
	}

	var listed []VolumeResponse
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listed) != 1 || listed[0].VolumeID != "test-volume" {
		t.Fatalf("expected created volume in list, got %#v", listed)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/volumes/test-volume", nil)
	getRec := httptest.NewRecorder()

	app.ServeHTTP(getRec, getReq)

	if getRec.Code != http.StatusOK {
		t.Fatalf("expected get status %d, got %d: %s", http.StatusOK, getRec.Code, getRec.Body.String())
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/volumes/test-volume", nil)
	deleteRec := httptest.NewRecorder()

	app.ServeHTTP(deleteRec, deleteReq)

	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("expected delete status %d, got %d: %s", http.StatusNoContent, deleteRec.Code, deleteRec.Body.String())
	}
	if len(runtime.deletedVolumeIDs) != 1 || runtime.deletedVolumeIDs[0] != "test-volume" {
		t.Fatalf("expected runtime delete volume call, got %#v", runtime.deletedVolumeIDs)
	}
}

func TestDeleteVolumePreservesConflictStatus(t *testing.T) {
	runtime := &recordingRuntime{
		volumes:         []RuntimeVolume{{VolumeID: "test-volume", Name: "test-volume"}},
		deleteVolumeErr: gatewayError(http.StatusConflict, "volume test-volume is in use"),
	}
	app, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/volumes/test-volume", nil)
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected delete status %d, got %d: %s", http.StatusConflict, rec.Code, rec.Body.String())
	}
	if len(runtime.deletedVolumeIDs) != 0 {
		t.Fatalf("expected volume deletion to be rejected, got %#v", runtime.deletedVolumeIDs)
	}
}

func TestVolumeContentEndpointsUseRuntime(t *testing.T) {
	runtime := &recordingRuntime{}
	app, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	runtime.volumes = append(runtime.volumes, RuntimeVolume{VolumeID: "test-volume", Name: "test-volume"})

	putReq := httptest.NewRequest(http.MethodPut, "/volumecontent/test-volume/file?path=manifest.json&force=true&mode=420", strings.NewReader(`{"skills":[]}`))
	putRec := httptest.NewRecorder()
	app.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("expected put file status %d, got %d: %s", http.StatusOK, putRec.Code, putRec.Body.String())
	}

	var putStat VolumeEntryStat
	if err := json.Unmarshal(putRec.Body.Bytes(), &putStat); err != nil {
		t.Fatalf("decode put stat: %v", err)
	}
	if putStat.Type != "file" || putStat.Path != "/manifest.json" || putStat.Mode != 420 {
		t.Fatalf("unexpected put stat: %#v", putStat)
	}

	octalReq := httptest.NewRequest(http.MethodPut, "/volumecontent/test-volume/file?path=octal.txt&force=true&mode=0640", strings.NewReader("octal"))
	octalRec := httptest.NewRecorder()
	app.ServeHTTP(octalRec, octalReq)
	if octalRec.Code != http.StatusOK {
		t.Fatalf("expected octal mode put status %d, got %d: %s", http.StatusOK, octalRec.Code, octalRec.Body.String())
	}
	var octalStat VolumeEntryStat
	if err := json.Unmarshal(octalRec.Body.Bytes(), &octalStat); err != nil {
		t.Fatalf("decode octal put stat: %v", err)
	}
	if octalStat.Mode != 0o640 {
		t.Fatalf("expected octal mode 0640, got %#v", octalStat)
	}

	prefixedReq := httptest.NewRequest(http.MethodPut, "/volumecontent/test-volume/file?path=prefixed.txt&force=true&mode=0o644", strings.NewReader("prefixed"))
	prefixedRec := httptest.NewRecorder()
	app.ServeHTTP(prefixedRec, prefixedReq)
	if prefixedRec.Code != http.StatusOK {
		t.Fatalf("expected prefixed octal mode put status %d, got %d: %s", http.StatusOK, prefixedRec.Code, prefixedRec.Body.String())
	}
	var prefixedStat VolumeEntryStat
	if err := json.Unmarshal(prefixedRec.Body.Bytes(), &prefixedStat); err != nil {
		t.Fatalf("decode prefixed octal put stat: %v", err)
	}
	if prefixedStat.Mode != 0o644 {
		t.Fatalf("expected prefixed octal mode 0o644, got %#v", prefixedStat)
	}

	ambiguousReq := httptest.NewRequest(http.MethodPut, "/volumecontent/test-volume/file?path=ambiguous.txt&force=true&mode=644", strings.NewReader("ambiguous"))
	ambiguousRec := httptest.NewRecorder()
	app.ServeHTTP(ambiguousRec, ambiguousReq)
	if ambiguousRec.Code != http.StatusBadRequest {
		t.Fatalf("expected unprefixed out-of-range mode status %d, got %d: %s", http.StatusBadRequest, ambiguousRec.Code, ambiguousRec.Body.String())
	}

	pathReq := httptest.NewRequest(http.MethodGet, "/volumecontent/test-volume/path?path=/manifest.json", nil)
	pathRec := httptest.NewRecorder()
	app.ServeHTTP(pathRec, pathReq)
	if pathRec.Code != http.StatusOK {
		t.Fatalf("expected path status %d, got %d: %s", http.StatusOK, pathRec.Code, pathRec.Body.String())
	}

	var pathStat VolumeEntryStat
	if err := json.Unmarshal(pathRec.Body.Bytes(), &pathStat); err != nil {
		t.Fatalf("decode path stat: %v", err)
	}
	if pathStat.Path != "/manifest.json" || pathStat.Size != int64(len(`{"skills":[]}`)) {
		t.Fatalf("unexpected path stat: %#v", pathStat)
	}

	fileReq := httptest.NewRequest(http.MethodGet, "/volumecontent/test-volume/file?path=manifest.json", nil)
	fileRec := httptest.NewRecorder()
	app.ServeHTTP(fileRec, fileReq)
	if fileRec.Code != http.StatusOK {
		t.Fatalf("expected file status %d, got %d: %s", http.StatusOK, fileRec.Code, fileRec.Body.String())
	}
	if fileRec.Body.String() != `{"skills":[]}` {
		t.Fatalf("unexpected file body %q", fileRec.Body.String())
	}

	dirReq := httptest.NewRequest(http.MethodPost, "/volumecontent/test-volume/dir?path=nested&mode=493", nil)
	dirRec := httptest.NewRecorder()
	app.ServeHTTP(dirRec, dirReq)
	if dirRec.Code != http.StatusOK {
		t.Fatalf("expected dir create status %d, got %d: %s", http.StatusOK, dirRec.Code, dirRec.Body.String())
	}

	deepPutReq := httptest.NewRequest(http.MethodPut, "/volumecontent/test-volume/file?path=nested/deep.txt&force=true", strings.NewReader("deep"))
	deepPutRec := httptest.NewRecorder()
	app.ServeHTTP(deepPutRec, deepPutReq)
	if deepPutRec.Code != http.StatusOK {
		t.Fatalf("expected nested file put status %d, got %d: %s", http.StatusOK, deepPutRec.Code, deepPutRec.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/volumecontent/test-volume/dir?path=/&depth=1", nil)
	listRec := httptest.NewRecorder()
	app.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected dir list status %d, got %d: %s", http.StatusOK, listRec.Code, listRec.Body.String())
	}
	var entries []VolumeEntryStat
	if err := json.Unmarshal(listRec.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode dir entries: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("expected written files and directory entries, got %#v", entries)
	}

	deepListReq := httptest.NewRequest(http.MethodGet, "/volumecontent/test-volume/dir?path=/&depth=2", nil)
	deepListRec := httptest.NewRecorder()
	app.ServeHTTP(deepListRec, deepListReq)
	if deepListRec.Code != http.StatusOK {
		t.Fatalf("expected recursive dir list status %d, got %d: %s", http.StatusOK, deepListRec.Code, deepListRec.Body.String())
	}
	var deepEntries []VolumeEntryStat
	if err := json.Unmarshal(deepListRec.Body.Bytes(), &deepEntries); err != nil {
		t.Fatalf("decode recursive dir entries: %v", err)
	}
	containsDeepEntry := false
	for _, entry := range deepEntries {
		containsDeepEntry = containsDeepEntry || entry.Path == "/nested/deep.txt"
	}
	if len(deepEntries) != 5 || !containsDeepEntry {
		t.Fatalf("expected depth=2 to include nested file, got %#v", deepEntries)
	}

	missingReq := httptest.NewRequest(http.MethodGet, "/volumecontent/test-volume/path?path=missing.json", nil)
	missingRec := httptest.NewRecorder()
	app.ServeHTTP(missingRec, missingReq)
	if missingRec.Code != http.StatusNotFound {
		t.Fatalf("expected missing path status %d, got %d: %s", http.StatusNotFound, missingRec.Code, missingRec.Body.String())
	}

	invalidReq := httptest.NewRequest(http.MethodPut, "/volumecontent/test-volume/file?path=manifest.json&force=not-bool", strings.NewReader("x"))
	invalidRec := httptest.NewRecorder()
	app.ServeHTTP(invalidRec, invalidReq)
	if invalidRec.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid force status %d, got %d: %s", http.StatusBadRequest, invalidRec.Code, invalidRec.Body.String())
	}

	missingPutReq := httptest.NewRequest(http.MethodPut, "/volumecontent/missing/file?path=manifest.json", strings.NewReader("x"))
	missingPutRec := httptest.NewRecorder()
	app.ServeHTTP(missingPutRec, missingPutReq)
	if missingPutRec.Code != http.StatusNotFound {
		t.Fatalf("expected missing volume put status %d, got %d: %s", http.StatusNotFound, missingPutRec.Code, missingPutRec.Body.String())
	}

	missingDirReq := httptest.NewRequest(http.MethodPost, "/volumecontent/missing/dir?path=nested", nil)
	missingDirRec := httptest.NewRecorder()
	app.ServeHTTP(missingDirRec, missingDirReq)
	if missingDirRec.Code != http.StatusNotFound {
		t.Fatalf("expected missing volume dir status %d, got %d: %s", http.StatusNotFound, missingDirRec.Code, missingDirRec.Body.String())
	}

	oldMaxVolumeFileUploadBytes := maxVolumeFileUploadBytes
	maxVolumeFileUploadBytes = 4
	t.Cleanup(func() {
		maxVolumeFileUploadBytes = oldMaxVolumeFileUploadBytes
	})
	tooLargeReq := httptest.NewRequest(http.MethodPut, "/volumecontent/test-volume/file?path=too-large.txt&force=true", strings.NewReader("12345"))
	tooLargeRec := httptest.NewRecorder()
	app.ServeHTTP(tooLargeRec, tooLargeReq)
	if tooLargeRec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected too large upload status %d, got %d: %s", http.StatusRequestEntityTooLarge, tooLargeRec.Code, tooLargeRec.Body.String())
	}
}

func TestVolumeContentEndpointsPreservePathWhitespace(t *testing.T) {
	runtime := &recordingRuntime{
		volumes: []RuntimeVolume{{VolumeID: "test-volume", Name: "test-volume"}},
	}
	app, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	for _, request := range []struct {
		path    string
		content string
	}{
		{path: "report.txt", content: "plain"},
		{path: "report.txt%20", content: "trailing-space"},
	} {
		req := httptest.NewRequest(http.MethodPut, "/volumecontent/test-volume/file?path="+request.path+"&force=true", strings.NewReader(request.content))
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("write %q status %d: %s", request.path, rec.Code, rec.Body.String())
		}
	}

	fileReq := httptest.NewRequest(http.MethodGet, "/volumecontent/test-volume/file?path=report.txt%20", nil)
	fileRec := httptest.NewRecorder()
	app.ServeHTTP(fileRec, fileReq)
	if fileRec.Code != http.StatusOK || fileRec.Body.String() != "trailing-space" {
		t.Fatalf("expected trailing-space file, got status %d body %q", fileRec.Code, fileRec.Body.String())
	}

	plainReq := httptest.NewRequest(http.MethodGet, "/volumecontent/test-volume/file?path=report.txt", nil)
	plainRec := httptest.NewRecorder()
	app.ServeHTTP(plainRec, plainReq)
	if plainRec.Code != http.StatusOK || plainRec.Body.String() != "plain" {
		t.Fatalf("expected plain file to remain distinct, got status %d body %q", plainRec.Code, plainRec.Body.String())
	}

	statReq := httptest.NewRequest(http.MethodGet, "/volumecontent/test-volume/path?path=report.txt%20", nil)
	statRec := httptest.NewRecorder()
	app.ServeHTTP(statRec, statReq)
	if statRec.Code != http.StatusOK {
		t.Fatalf("stat trailing-space file status %d: %s", statRec.Code, statRec.Body.String())
	}
	var stat VolumeEntryStat
	if err := json.Unmarshal(statRec.Body.Bytes(), &stat); err != nil {
		t.Fatalf("decode trailing-space stat: %v", err)
	}
	if stat.Name != "report.txt " || stat.Path != "/report.txt " {
		t.Fatalf("stat changed trailing-space path: %#v", stat)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/volumecontent/test-volume/dir?path=/&depth=1", nil)
	listRec := httptest.NewRecorder()
	app.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list whitespace-sensitive files status %d: %s", listRec.Code, listRec.Body.String())
	}
	var entries []VolumeEntryStat
	if err := json.Unmarshal(listRec.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode whitespace-sensitive entries: %v", err)
	}
	if len(entries) != 2 || entries[0].Path != "/report.txt" || entries[1].Path != "/report.txt " {
		t.Fatalf("expected distinct whitespace-sensitive entries, got %#v", entries)
	}
}

func TestVolumeContentGetEndpointsPreserveInternalErrors(t *testing.T) {
	runtime := &volumeContentErrorRuntime{err: errors.New("volume content failed")}
	app, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	for _, tt := range []struct {
		name   string
		method string
		path   string
	}{
		{name: "path", method: http.MethodGet, path: "/volumecontent/test-volume/path?path=manifest.json"},
		{name: "file", method: http.MethodGet, path: "/volumecontent/test-volume/file?path=manifest.json"},
		{name: "dir", method: http.MethodGet, path: "/volumecontent/test-volume/dir?path=/"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := httptest.NewRecorder()
			app.ServeHTTP(rec, req)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("expected status %d, got %d: %s", http.StatusInternalServerError, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestVolumeContentFileGetDoesNotAppendJSONAfterStreamingFailure(t *testing.T) {
	runtime := &volumeContentErrorRuntime{
		body: &gatewayFailingReadCloser{data: []byte("partial")},
	}
	app, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/volumecontent/test-volume/file?path=manifest.json", nil)
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected committed stream status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "partial" {
		t.Fatalf("expected only partial file bytes, got %q", rec.Body.String())
	}
}

func TestGoSDKVolumeContentFlow(t *testing.T) {
	runtime := &recordingRuntime{}
	app, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	server := httptest.NewServer(app)
	defer server.Close()

	ctx := context.Background()
	vol, err := sdkvolume.Create(ctx, "sdk-volume", &sdkvolume.ConnectionOpts{
		ApiKey: "e2b_0000000000000000000000000000000000000000",
		ApiUrl: server.URL,
	})
	if err != nil {
		t.Fatalf("sdk create volume: %v", err)
	}

	force := true
	if _, err := vol.WriteFile(ctx, "manifest.json", `{"skills":[]}`, &sdkvolume.VolumeWriteOptions{Force: &force}); err != nil {
		t.Fatalf("sdk write file: %v", err)
	}

	readValue, err := vol.ReadFile(ctx, "/manifest.json", nil)
	if err != nil {
		t.Fatalf("sdk read file: %v", err)
	}
	if readValue != `{"skills":[]}` {
		t.Fatalf("unexpected sdk read value %#v", readValue)
	}

	exists, err := vol.Exists(ctx, "manifest.json", nil)
	if err != nil {
		t.Fatalf("sdk exists: %v", err)
	}
	if !exists {
		t.Fatal("expected manifest.json to exist")
	}

	exists, err = vol.Exists(ctx, "missing.json", nil)
	if err != nil {
		t.Fatalf("sdk missing exists: %v", err)
	}
	if exists {
		t.Fatal("expected missing.json not to exist")
	}
}

func TestDockerImageReferenceRequiresTagOrDigest(t *testing.T) {
	tests := []struct {
		ref  string
		want bool
	}{
		{ref: "", want: false},
		{ref: "base", want: false},
		{ref: "code-interpreter", want: false},
		{ref: "ubuntu:22.04", want: true},
		{ref: "registry.example.com/team/runtime:latest", want: true},
		{ref: "registry.example.com:5000/team/runtime:latest", want: true},
		{ref: "registry.example.com/team/runtime@sha256:abc", want: true},
	}

	for _, tt := range tests {
		if got := isDockerImageReference(tt.ref); got != tt.want {
			t.Fatalf("isDockerImageReference(%q) = %v, want %v", tt.ref, got, tt.want)
		}
	}
}

func TestShortDockerImageName(t *testing.T) {
	tests := []struct {
		imageRef string
		want     string
	}{
		{imageRef: "registry.example.com/ccloud/claude-code-interpreter:latest", want: "claude-code-interpreter:latest"},
		{imageRef: "postgres:17.4", want: "postgres:17.4"},
		{imageRef: "registry.example.com:5000/team/runtime:latest", want: "runtime:latest"},
		{imageRef: "registry.example.com/team/runtime@sha256:abc", want: "runtime@sha256:abc"},
	}

	for _, tt := range tests {
		if got := shortDockerImageName(tt.imageRef); got != tt.want {
			t.Fatalf("shortDockerImageName(%q) = %q, want %q", tt.imageRef, got, tt.want)
		}
	}
}

func TestDockerTemplateNameDropsTagAndRegistry(t *testing.T) {
	tests := []struct {
		imageRef string
		want     string
	}{
		{imageRef: "registry.example.com/ccloud/claude-code-interpreter:latest", want: "claude-code-interpreter"},
		{imageRef: "postgres:17.4", want: "postgres"},
		{imageRef: "registry.example.com:5000/team/runtime:latest", want: "runtime"},
		{imageRef: "registry.example.com/team/runtime@sha256:abc", want: "runtime"},
	}

	for _, tt := range tests {
		if got := dockerTemplateName(tt.imageRef); got != tt.want {
			t.Fatalf("dockerTemplateName(%q) = %q, want %q", tt.imageRef, got, tt.want)
		}
	}
}

func TestHealthz(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health response: %v", err)
	}

	if body["status"] != "ok" {
		t.Fatalf("expected status ok, got %q", body["status"])
	}
}

func TestHealth(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "Health check successful" {
		t.Fatalf("unexpected health body %q", rec.Body.String())
	}
}

func TestSandboxLogsUseCallbacks(t *testing.T) {
	app, err := NewAppWithCallbacks(DefaultConfig(), log.New(io.Discard, "", 0), &recordingRuntime{}, GatewayCallbacks{
		GetSandboxLogs: func(ctx context.Context, sandboxID string, req SandboxLogsRequest) ([]SandboxRuntimeLogEntry, error) {
			return []SandboxRuntimeLogEntry{
				{
					Timestamp: time.Unix(1, 0).UTC(),
					Level:     e2bapi.LogLevelInfo,
					Message:   "v1 log",
				},
			}, nil
		},
		GetSandboxLogsV2: func(ctx context.Context, sandboxID string, req SandboxLogsRequest) ([]SandboxRuntimeLogEntry, error) {
			return []SandboxRuntimeLogEntry{
				{
					Timestamp: time.Unix(2, 0).UTC(),
					Level:     e2bapi.LogLevelWarn,
					Message:   "v2 log",
				},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"base"}`))
	createRec := httptest.NewRecorder()
	app.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}
	var created SandboxResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	v1Req := httptest.NewRequest(http.MethodGet, "/sandboxes/"+created.SandboxID+"/logs?limit=10", nil)
	v1Rec := httptest.NewRecorder()
	app.ServeHTTP(v1Rec, v1Req)
	if v1Rec.Code != http.StatusOK {
		t.Fatalf("expected v1 logs status %d, got %d: %s", http.StatusOK, v1Rec.Code, v1Rec.Body.String())
	}
	var v1Logs e2bapi.SandboxLogs
	if err := json.Unmarshal(v1Rec.Body.Bytes(), &v1Logs); err != nil {
		t.Fatalf("decode v1 logs: %v", err)
	}
	if len(v1Logs.Logs) != 1 || v1Logs.Logs[0].Line != "v1 log" {
		t.Fatalf("expected v1 callback log, got %#v", v1Logs)
	}

	v2Req := httptest.NewRequest(http.MethodGet, "/v2/sandboxes/"+created.SandboxID+"/logs?limit=10&level=warn", nil)
	v2Rec := httptest.NewRecorder()
	app.ServeHTTP(v2Rec, v2Req)
	if v2Rec.Code != http.StatusOK {
		t.Fatalf("expected v2 logs status %d, got %d: %s", http.StatusOK, v2Rec.Code, v2Rec.Body.String())
	}
	var v2Logs e2bapi.SandboxLogsV2Response
	if err := json.Unmarshal(v2Rec.Body.Bytes(), &v2Logs); err != nil {
		t.Fatalf("decode v2 logs: %v", err)
	}
	if len(v2Logs.Logs) != 1 || v2Logs.Logs[0].Message != "v2 log" || v2Logs.Logs[0].Level != e2bapi.LogLevelWarn {
		t.Fatalf("expected v2 callback log, got %#v", v2Logs)
	}
}

func TestSandboxLogsDefaultLimitMatchesOpenAPI(t *testing.T) {
	var seenV1Limit int32
	var seenV2Limit int32
	app, err := NewAppWithCallbacks(DefaultConfig(), log.New(io.Discard, "", 0), &recordingRuntime{}, GatewayCallbacks{
		GetSandboxLogs: func(ctx context.Context, sandboxID string, req SandboxLogsRequest) ([]SandboxRuntimeLogEntry, error) {
			seenV1Limit = req.Limit
			return nil, nil
		},
		GetSandboxLogsV2: func(ctx context.Context, sandboxID string, req SandboxLogsRequest) ([]SandboxRuntimeLogEntry, error) {
			seenV2Limit = req.Limit
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"base"}`))
	createRec := httptest.NewRecorder()
	app.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}
	var created SandboxResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	v1Req := httptest.NewRequest(http.MethodGet, "/sandboxes/"+created.SandboxID+"/logs", nil)
	v1Rec := httptest.NewRecorder()
	app.ServeHTTP(v1Rec, v1Req)
	if v1Rec.Code != http.StatusOK {
		t.Fatalf("expected v1 logs status %d, got %d: %s", http.StatusOK, v1Rec.Code, v1Rec.Body.String())
	}

	v2Req := httptest.NewRequest(http.MethodGet, "/v2/sandboxes/"+created.SandboxID+"/logs", nil)
	v2Rec := httptest.NewRecorder()
	app.ServeHTTP(v2Rec, v2Req)
	if v2Rec.Code != http.StatusOK {
		t.Fatalf("expected v2 logs status %d, got %d: %s", http.StatusOK, v2Rec.Code, v2Rec.Body.String())
	}

	if seenV1Limit != 1000 || seenV2Limit != 1000 {
		t.Fatalf("expected default log limits to be 1000, got v1=%d v2=%d", seenV1Limit, seenV2Limit)
	}
}

func TestGeneratedNodesRouteReturnsLocalNode(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	req := httptest.NewRequest(http.MethodGet, "/nodes", nil)
	rec := httptest.NewRecorder()

	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var nodes []e2bapi.Node
	if err := json.Unmarshal(rec.Body.Bytes(), &nodes); err != nil {
		t.Fatalf("decode nodes response: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Id != localNodeID {
		t.Fatalf("unexpected nodes response: %#v", nodes)
	}
}

func TestOptionsPreflight(t *testing.T) {
	app := newTestApp(t, DefaultConfig())

	req := httptest.NewRequest(http.MethodOptions, "/sandboxes", nil)
	rec := httptest.NewRecorder()

	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d", http.StatusNoContent, rec.Code)
	}

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("expected CORS allow origin *, got %q", got)
	}
}

type recordingRuntime struct {
	createCalls         []SandboxRuntimeCreateRequest
	deleteInfos         []SandboxRuntimeInfo
	pauseInfos          []SandboxRuntimeInfo
	resumeInfos         []SandboxRuntimeInfo
	snapshotCreateCalls []recordingSnapshotCreateCall
	snapshotListCalls   []SnapshotListRequest
	snapshots           []e2bapi.SnapshotInfo
	templates           []SandboxRuntimeTemplate
	volumes             []RuntimeVolume
	volumeFiles         map[string]recordingVolumeContentEntry
	volumeDirs          map[string]recordingVolumeContentEntry
	createRuntimeInfo   SandboxRuntimeInfo
	deletedVolumeIDs    []string
	deleteSandboxErr    error
	deleteVolumeErr     error
	inspectCalls        []SandboxRuntimeInfo
	inspectResults      map[string]SandboxRuntimeInspection
	inspectErr          error
}

type restoringRuntime struct {
	recordingRuntime
	restoreRecords []SandboxRecord
	restoreCalls   int
}

type recordingTemplateBuilderRuntime struct {
	recordingRuntime
	buildTemplates      []GatewayTemplate
	buildRequests       []e2bapi.TemplateBuildRequest
	startBuildTemplates []GatewayTemplate
	startBuildIDs       []string
	startBuildRequests  []e2bapi.TemplateBuildStartV2
	startBuildFiles     [][]TemplateBuildFile
	startBuildEntered   chan struct{}
	releaseStartBuild   chan struct{}
	startBuildReturned  chan struct{}
}

type volumeContentErrorRuntime struct {
	recordingRuntime
	err  error
	body io.ReadCloser
}

type recordingSnapshotCreateCall struct {
	Record  SandboxRecord
	Request e2bapi.PostSandboxesSandboxIDSnapshotsJSONBody
}

type recordingVolumeContentEntry struct {
	stat VolumeEntryStat
	data []byte
}

func (r *recordingRuntime) CreateSandbox(ctx context.Context, req SandboxRuntimeCreateRequest) (SandboxRuntimeInfo, error) {
	r.createCalls = append(r.createCalls, req)
	info := r.createRuntimeInfo
	if info.EnvdURL == "" {
		info.EnvdURL = "http://127.0.0.1:50000"
	}
	if info.ContainerID == "" {
		info.ContainerID = "ctr-" + req.SandboxID
	}
	if info.ContainerName == "" {
		info.ContainerName = "e2b-envd-" + req.SandboxID
	}
	if info.HostPort == "" {
		info.HostPort = "50000"
	}
	if len(info.VolumeMounts) == 0 {
		info.VolumeMounts = req.VolumeMounts
	}
	return info, nil
}

func (r *recordingRuntime) DeleteSandbox(ctx context.Context, info SandboxRuntimeInfo) error {
	r.deleteInfos = append(r.deleteInfos, info)
	return r.deleteSandboxErr
}

func (r *recordingRuntime) ListTemplates(ctx context.Context) ([]SandboxRuntimeTemplate, error) {
	if r.templates != nil {
		return r.templates, nil
	}

	now := time.Now().UTC()
	return []SandboxRuntimeTemplate{
		{
			TemplateID:  "base",
			Names:       []string{"base"},
			BuildCount:  1,
			BuildID:     "base",
			BuildStatus: "ready",
			CPUCount:    1,
			MemoryMB:    512,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
	}, nil
}

func (r *recordingRuntime) PauseSandbox(ctx context.Context, info SandboxRuntimeInfo) error {
	r.pauseInfos = append(r.pauseInfos, info)
	return nil
}

func (r *recordingRuntime) ResumeSandbox(ctx context.Context, info SandboxRuntimeInfo) (SandboxRuntimeInfo, error) {
	r.resumeInfos = append(r.resumeInfos, info)
	return info, nil
}

func (r *recordingRuntime) InspectSandbox(ctx context.Context, info SandboxRuntimeInfo) (SandboxRuntimeInspection, error) {
	r.inspectCalls = append(r.inspectCalls, info)
	if r.inspectErr != nil {
		return SandboxRuntimeInspection{}, r.inspectErr
	}
	if r.inspectResults != nil {
		if result, ok := r.inspectResults[info.ContainerID]; ok {
			return result, nil
		}
		if result, ok := r.inspectResults[info.ContainerName]; ok {
			return result, nil
		}
	}
	return SandboxRuntimeInspection{
		Info:   info,
		State:  "",
		Exists: true,
	}, nil
}

func (r *restoringRuntime) RestoreSandboxes(ctx context.Context) ([]SandboxRecord, error) {
	r.restoreCalls++
	return append([]SandboxRecord(nil), r.restoreRecords...), nil
}

func (r *recordingTemplateBuilderRuntime) BuildTemplate(ctx context.Context, template GatewayTemplate, req e2bapi.TemplateBuildRequest) (GatewayTemplate, []e2bapi.BuildLogEntry, error) {
	r.buildTemplates = append(r.buildTemplates, template)
	r.buildRequests = append(r.buildRequests, req)
	template.ImageRef = "registry.example.test/" + template.TemplateID + ":latest"
	template.BuildID = "runtime-build"
	template.BuildStatus = e2bapi.TemplateBuildStatusReady
	template.DiskSizeMB = 64
	return template, localBuildLogs("runtime builder completed"), nil
}

func (r *recordingTemplateBuilderRuntime) StartTemplateBuildV2(ctx context.Context, template GatewayTemplate, buildID string, req e2bapi.TemplateBuildStartV2, files []TemplateBuildFile) (GatewayTemplate, []e2bapi.BuildLogEntry, error) {
	if r.startBuildReturned != nil {
		defer close(r.startBuildReturned)
	}
	r.startBuildTemplates = append(r.startBuildTemplates, template)
	r.startBuildIDs = append(r.startBuildIDs, buildID)
	r.startBuildRequests = append(r.startBuildRequests, req)
	r.startBuildFiles = append(r.startBuildFiles, append([]TemplateBuildFile(nil), files...))
	if r.startBuildEntered != nil {
		close(r.startBuildEntered)
	}
	if r.releaseStartBuild != nil {
		select {
		case <-r.releaseStartBuild:
		case <-ctx.Done():
			return GatewayTemplate{}, nil, ctx.Err()
		}
	}
	template.ImageRef = "registry.example.test/" + template.TemplateID + ":latest"
	template.BuildID = buildID
	template.BuildStatus = e2bapi.TemplateBuildStatusReady
	template.DiskSizeMB = 128
	return template, localBuildLogs("runtime v2 builder completed"), nil
}

func (r *recordingRuntime) CreateSandboxSnapshot(ctx context.Context, record SandboxRecord, req e2bapi.PostSandboxesSandboxIDSnapshotsJSONBody) (e2bapi.SnapshotInfo, error) {
	r.snapshotCreateCalls = append(r.snapshotCreateCalls, recordingSnapshotCreateCall{
		Record:  record,
		Request: req,
	})
	snapshot := e2bapi.SnapshotInfo{
		SnapshotID: "snapshot-" + record.ID,
		Names:      []string{"snapshot-" + record.ID},
	}
	r.snapshots = append(r.snapshots, snapshot)
	return snapshot, nil
}

func (r *recordingRuntime) ListSnapshots(ctx context.Context, req SnapshotListRequest) ([]e2bapi.SnapshotInfo, error) {
	r.snapshotListCalls = append(r.snapshotListCalls, req)
	return append([]e2bapi.SnapshotInfo(nil), r.snapshots...), nil
}

func (r *recordingRuntime) CreateVolume(ctx context.Context, name string) (RuntimeVolume, error) {
	volume := RuntimeVolume{
		VolumeID: name,
		Name:     name,
	}
	r.volumes = append(r.volumes, volume)
	return volume, nil
}

func (r *recordingRuntime) ListVolumes(ctx context.Context) ([]RuntimeVolume, error) {
	return append([]RuntimeVolume(nil), r.volumes...), nil
}

func (r *recordingRuntime) GetVolume(ctx context.Context, volumeID string) (RuntimeVolume, error) {
	for _, volume := range r.volumes {
		if volume.VolumeID == volumeID {
			return volume, nil
		}
	}
	return RuntimeVolume{}, errdefs.NotFound(errors.New("volume not found"))
}

func (r *recordingRuntime) DeleteVolume(ctx context.Context, volumeID string) (bool, error) {
	if r.deleteVolumeErr != nil {
		return false, r.deleteVolumeErr
	}
	for i, volume := range r.volumes {
		if volume.VolumeID == volumeID {
			r.volumes = append(r.volumes[:i], r.volumes[i+1:]...)
			r.deletedVolumeIDs = append(r.deletedVolumeIDs, volumeID)
			return true, nil
		}
	}
	return false, nil
}

func (r *recordingRuntime) GetVolumePathInfo(ctx context.Context, volumeID string, path string) (VolumeEntryStat, error) {
	if _, err := r.GetVolume(ctx, volumeID); err != nil {
		return VolumeEntryStat{}, err
	}
	key := recordingVolumeContentKey(volumeID, path)
	if key == recordingVolumeContentKey(volumeID, "") {
		return recordingVolumeContentStat("", "directory", 0, 0o755), nil
	}
	if entry, ok := r.volumeFiles[key]; ok {
		return entry.stat, nil
	}
	if entry, ok := r.volumeDirs[key]; ok {
		return entry.stat, nil
	}
	return VolumeEntryStat{}, errdefs.NotFound(errors.New("volume path not found"))
}

func (r *volumeContentErrorRuntime) GetVolumePathInfo(ctx context.Context, volumeID string, path string) (VolumeEntryStat, error) {
	if r.err != nil {
		return VolumeEntryStat{}, r.err
	}
	return r.recordingRuntime.GetVolumePathInfo(ctx, volumeID, path)
}

func (r *recordingRuntime) ReadVolumeFile(ctx context.Context, volumeID string, path string) (io.ReadCloser, error) {
	if _, err := r.GetVolume(ctx, volumeID); err != nil {
		return nil, err
	}
	entry, ok := r.volumeFiles[recordingVolumeContentKey(volumeID, path)]
	if !ok {
		return nil, errdefs.NotFound(errors.New("volume file not found"))
	}
	return io.NopCloser(bytes.NewReader(entry.data)), nil
}

func (r *volumeContentErrorRuntime) ReadVolumeFile(ctx context.Context, volumeID string, path string) (io.ReadCloser, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.body != nil {
		return r.body, nil
	}
	return r.recordingRuntime.ReadVolumeFile(ctx, volumeID, path)
}

func (r *recordingRuntime) WriteVolumeFile(ctx context.Context, volumeID string, path string, body io.Reader, opts VolumeWriteOptions) (VolumeEntryStat, error) {
	if _, err := r.GetVolume(ctx, volumeID); err != nil {
		return VolumeEntryStat{}, err
	}
	if r.volumeFiles == nil {
		r.volumeFiles = map[string]recordingVolumeContentEntry{}
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return VolumeEntryStat{}, err
	}
	mode := 0o644
	if opts.Mode != nil {
		mode = *opts.Mode
	}
	stat := recordingVolumeContentStat(path, "file", int64(len(data)), mode)
	r.volumeFiles[recordingVolumeContentKey(volumeID, path)] = recordingVolumeContentEntry{stat: stat, data: data}
	return stat, nil
}

func (r *recordingRuntime) ListVolumeDir(ctx context.Context, volumeID string, path string, depth int) ([]VolumeEntryStat, error) {
	if _, err := r.GetVolume(ctx, volumeID); err != nil {
		return nil, err
	}
	prefix := recordingVolumeContentPath(path)
	var entries []VolumeEntryStat
	for key, entry := range r.volumeFiles {
		entryVolumeID, entryPath := recordingVolumeContentKeyParts(key)
		if entryVolumeID == volumeID && recordingVolumeContentWithinDepth(prefix, entryPath, depth) {
			entries = append(entries, entry.stat)
		}
	}
	for key, entry := range r.volumeDirs {
		entryVolumeID, entryPath := recordingVolumeContentKeyParts(key)
		if entryVolumeID == volumeID && recordingVolumeContentWithinDepth(prefix, entryPath, depth) {
			entries = append(entries, entry.stat)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})
	return entries, nil
}

func (r *volumeContentErrorRuntime) ListVolumeDir(ctx context.Context, volumeID string, path string, depth int) ([]VolumeEntryStat, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.recordingRuntime.ListVolumeDir(ctx, volumeID, path, depth)
}

func (r *recordingRuntime) CreateVolumeDir(ctx context.Context, volumeID string, path string, opts VolumeWriteOptions) (VolumeEntryStat, error) {
	if _, err := r.GetVolume(ctx, volumeID); err != nil {
		return VolumeEntryStat{}, err
	}
	if r.volumeDirs == nil {
		r.volumeDirs = map[string]recordingVolumeContentEntry{}
	}
	mode := 0o755
	if opts.Mode != nil {
		mode = *opts.Mode
	}
	stat := recordingVolumeContentStat(path, "directory", 0, mode)
	r.volumeDirs[recordingVolumeContentKey(volumeID, path)] = recordingVolumeContentEntry{stat: stat}
	return stat, nil
}

func recordingVolumeContentKey(volumeID string, path string) string {
	return volumeID + "\x00" + recordingVolumeContentPath(path)
}

func recordingVolumeContentKeyParts(key string) (string, string) {
	parts := strings.SplitN(key, "\x00", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func recordingVolumeContentPath(path string) string {
	path = strings.ReplaceAll(path, "\\", "/")
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")
	return path
}

func recordingVolumeContentWithinDepth(parent string, child string, depth int) bool {
	if depth <= 0 {
		depth = 1
	}
	descendant := child
	if parent != "" {
		if !strings.HasPrefix(child, parent+"/") {
			return false
		}
		descendant = strings.TrimPrefix(child, parent+"/")
	}
	return descendant != "" && strings.Count(descendant, "/") < depth
}

func recordingVolumeContentStat(path string, entryType string, size int64, mode int) VolumeEntryStat {
	cleanPath := recordingVolumeContentPath(path)
	name := cleanPath
	if name == "" {
		name = "/"
	} else if index := strings.LastIndex(name, "/"); index >= 0 {
		name = name[index+1:]
	}
	now := time.Now().UTC()
	return VolumeEntryStat{
		Atime: now,
		Mtime: now,
		Ctime: now,
		Type:  entryType,
		Name:  name,
		Path:  "/" + cleanPath,
		Size:  size,
		Mode:  mode,
	}
}

type gatewayFailingReadCloser struct {
	data []byte
	done bool
}

func (r *gatewayFailingReadCloser) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.data), nil
	}
	return 0, errors.New("stream failed")
}

func (r *gatewayFailingReadCloser) Close() error {
	return nil
}

func waitForSignal(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func createSandboxForTest(t *testing.T, app http.Handler, templateID string, expectedStatus int) SandboxResponse {
	t.Helper()

	body := fmt.Sprintf(`{"templateID":%q}`, templateID)
	req := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)

	if rec.Code != expectedStatus {
		t.Fatalf("expected create status %d, got %d: %s", expectedStatus, rec.Code, rec.Body.String())
	}
	if expectedStatus != http.StatusCreated {
		return SandboxResponse{}
	}

	var sandbox SandboxResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &sandbox); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	return sandbox
}

func buildLogMessagesContain(logs []e2bapi.BuildLogEntry, message string) bool {
	for _, logEntry := range logs {
		if logEntry.Message == message {
			return true
		}
	}
	return false
}

func templateBuildListContains(builds []e2bapi.TemplateBuild, buildID string) bool {
	for _, build := range builds {
		if build.BuildID.String() == buildID {
			return true
		}
	}
	return false
}

func templateTagsContainBuildID(tags []e2bapi.TemplateTag, buildID string) bool {
	for _, tag := range tags {
		if tag.BuildID.String() == buildID {
			return true
		}
	}
	return false
}

func generatedServerInterfaceMethods(t *testing.T) []string {
	t.Helper()

	file := parseGoFile(t, filepath.Join("..", "e2bapi", "api.gen.go"))
	var methods []string
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range genDecl.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != "ServerInterface" {
				continue
			}
			interfaceType, ok := typeSpec.Type.(*ast.InterfaceType)
			if !ok {
				t.Fatalf("ServerInterface is not an interface")
			}
			for _, field := range interfaceType.Methods.List {
				for _, name := range field.Names {
					methods = append(methods, name.Name)
				}
			}
		}
	}

	if len(methods) == 0 {
		t.Fatalf("no ServerInterface methods found")
	}
	sort.Strings(methods)
	return methods
}

func explicitAppMethods(t *testing.T) map[string]bool {
	t.Helper()

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob go files: %v", err)
	}

	methods := map[string]bool{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file := parseGoFile(t, path)
		for _, decl := range file.Decls {
			funcDecl, ok := decl.(*ast.FuncDecl)
			if !ok || funcDecl.Recv == nil || len(funcDecl.Recv.List) == 0 {
				continue
			}
			if receiverTypeName(funcDecl.Recv.List[0].Type) == "App" {
				methods[funcDecl.Name.Name] = true
			}
		}
	}
	return methods
}

func parseGoFile(t *testing.T, path string) *ast.File {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return file
}

func TestListedSandboxResponseUsesRecordEndAt(t *testing.T) {
	app := &App{cfg: DefaultConfig()}

	createdAt := time.Now().UTC().Add(-10 * time.Minute)
	customEndAt := createdAt.Add(30 * time.Minute)

	record := SandboxRecord{
		ID:         "sbx_test_endat",
		TemplateID: "base",
		CreatedAt:  createdAt,
		EndAt:      customEndAt,
		State:      "running",
	}

	items := app.apiListedSandboxes([]SandboxRecord{record})
	if len(items) != 1 {
		t.Fatalf("expected one listed sandbox response, got %#v", items)
	}
	resp := items[0]

	if !resp.EndAt.Equal(customEndAt) {
		t.Fatalf("expected EndAt %s from record, got %s (hardcoded would be %s)", customEndAt, resp.EndAt, createdAt.Add(5*time.Minute))
	}

	hardcodedEndAt := createdAt.Add(5 * time.Minute)
	if resp.EndAt.Equal(hardcodedEndAt) {
		t.Fatal("EndAt must not be hardcoded to CreatedAt + 5 minutes")
	}
}

func receiverTypeName(expr ast.Expr) string {
	switch value := expr.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.StarExpr:
		return receiverTypeName(value.X)
	default:
		return ""
	}
}

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"e2b-local/internal/e2bapi"
)

type deadlineRuntime struct {
	recordingRuntime
	deleted chan SandboxRuntimeInfo
}

type blockingDeleteRuntime struct {
	recordingRuntime
	blockContainerID string
	deleteStarted    chan struct{}
	releaseDelete    chan struct{}
}

func (r *blockingDeleteRuntime) DeleteSandbox(ctx context.Context, info SandboxRuntimeInfo) error {
	if info.ContainerID != r.blockContainerID {
		return nil
	}
	select {
	case <-r.deleteStarted:
	default:
		close(r.deleteStarted)
	}
	select {
	case <-r.releaseDelete:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newDeadlineRuntime() *deadlineRuntime {
	return &deadlineRuntime{
		deleted: make(chan SandboxRuntimeInfo, 8),
	}
}

func (r *deadlineRuntime) DeleteSandbox(ctx context.Context, info SandboxRuntimeInfo) error {
	select {
	case r.deleted <- info:
	default:
	}
	return nil
}

func newDeadlineTestApp(t *testing.T) (*App, *deadlineRuntime) {
	t.Helper()

	runtime := newDeadlineRuntime()
	handler, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	app, ok := handler.(*App)
	if !ok {
		t.Fatalf("expected *App, got %T", handler)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := app.Shutdown(ctx); err != nil {
			t.Errorf("shutdown app: %v", err)
		}
	})
	return app, runtime
}

func sandboxDeadlineForTest(app *App, sandboxID string) (sandboxDeadline, bool) {
	app.deadlines.mu.Lock()
	defer app.deadlines.mu.Unlock()
	deadline, exists := app.deadlines.timers[sandboxID]
	return deadline, exists
}

func setSandboxEndAtForTest(t *testing.T, app *App, sandboxID string, endAt time.Time) {
	t.Helper()

	entry, exists := app.store.lockSandbox(sandboxID)
	if !exists {
		t.Fatalf("sandbox %q not found", sandboxID)
	}
	defer entry.lifecycleMu.Unlock()

	record, ok, err := app.store.SetEndAt(sandboxID, endAt)
	if err != nil || !ok {
		t.Fatalf("set deadline: ok=%t err=%v", ok, err)
	}
	app.syncSandboxDeadline(record)
}

func setSandboxEndAtWithoutReschedulingForTest(t *testing.T, app *App, sandboxID string, endAt time.Time) {
	t.Helper()

	entry, exists := app.store.lockSandbox(sandboxID)
	if !exists {
		t.Fatalf("sandbox %q not found", sandboxID)
	}
	defer entry.lifecycleMu.Unlock()

	if _, ok, err := app.store.SetEndAt(sandboxID, endAt); err != nil || !ok {
		t.Fatalf("set deadline without rescheduling: ok=%t err=%v", ok, err)
	}
}

func TestUnchangedInspectionDoesNotReplaceDeadline(t *testing.T) {
	app, _ := newDeadlineTestApp(t)
	created := createSandboxForTest(t, app, "base", http.StatusCreated)

	before, exists := sandboxDeadlineForTest(app, created.SandboxID)
	if !exists {
		t.Fatal("expected created sandbox deadline")
	}

	req := httptest.NewRequest(http.MethodGet, "/sandboxes/"+created.SandboxID, nil)
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected get status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	after, exists := sandboxDeadlineForTest(app, created.SandboxID)
	if !exists {
		t.Fatal("expected deadline to remain after inspection")
	}
	if after.timer != before.timer {
		t.Fatal("expected unchanged inspection to keep the existing timer")
	}
}

func TestSandboxDeadlineActivelyDeletesAtEndAt(t *testing.T) {
	app, runtime := newDeadlineTestApp(t)
	created := createSandboxForTest(t, app, "base", http.StatusCreated)

	setSandboxEndAtForTest(t, app, created.SandboxID, time.Now().UTC().Add(30*time.Millisecond))
	select {
	case deleted := <-runtime.deleted:
		if deleted.ContainerID != "ctr-"+created.SandboxID {
			t.Fatalf("expected deleted sandbox %q, got %#v", created.SandboxID, deleted)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active sandbox expiry")
	}

	if _, exists := app.store.Get(created.SandboxID); exists {
		t.Fatal("expected expired sandbox to be removed from store")
	}
}

func TestCreateUsesAPIDefaultTimeoutWhenOmitted(t *testing.T) {
	app, _ := newDeadlineTestApp(t)

	before := time.Now().UTC()
	created := createSandboxForTest(t, app, "base", http.StatusCreated)
	after := time.Now().UTC()

	record, exists := app.store.Get(created.SandboxID)
	if !exists {
		t.Fatal("expected created sandbox in store")
	}
	minEndAt := before.Add(time.Duration(DefaultSandboxTimeoutSeconds-1) * time.Second)
	maxEndAt := after.Add(time.Duration(DefaultSandboxTimeoutSeconds+1) * time.Second)
	if record.EndAt.Before(minEndAt) || record.EndAt.After(maxEndAt) {
		t.Fatalf("expected create to use the API default timeout, got %s", record.EndAt)
	}
}

func TestSetTimeoutReplacesEarlierDeadline(t *testing.T) {
	app, runtime := newDeadlineTestApp(t)
	created := createSandboxForTest(t, app, "base", http.StatusCreated)

	setSandboxEndAtForTest(t, app, created.SandboxID, time.Now().UTC().Add(40*time.Millisecond))
	req := httptest.NewRequest(
		http.MethodPost,
		"/sandboxes/"+created.SandboxID+"/timeout",
		bytes.NewBufferString(`{"timeout":1}`),
	)
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected timeout status %d, got %d: %s", http.StatusNoContent, rec.Code, rec.Body.String())
	}

	select {
	case deleted := <-runtime.deleted:
		t.Fatalf("old deadline deleted rescheduled sandbox: %#v", deleted)
	case <-time.After(120 * time.Millisecond):
	}

	record, exists := app.store.Get(created.SandboxID)
	if !exists {
		t.Fatal("expected rescheduled sandbox to remain in store")
	}
	if time.Until(record.EndAt) < 700*time.Millisecond {
		t.Fatalf("expected deadline to be reset near now+1s, got %s", record.EndAt)
	}
}

func TestConnectExtendsRunningSandboxDeadline(t *testing.T) {
	app, _ := newDeadlineTestApp(t)
	created := createSandboxForTest(t, app, "base", http.StatusCreated)

	setSandboxEndAtForTest(t, app, created.SandboxID, time.Now().UTC().Add(100*time.Millisecond))
	req := httptest.NewRequest(
		http.MethodPost,
		"/sandboxes/"+created.SandboxID+"/connect",
		bytes.NewBufferString(`{"timeout":1}`),
	)
	rec := httptest.NewRecorder()
	before := time.Now().UTC()
	app.ServeHTTP(rec, req)
	after := time.Now().UTC()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected connect status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	record, exists := app.store.Get(created.SandboxID)
	if !exists {
		t.Fatal("expected connected sandbox in store")
	}
	if record.EndAt.Before(before.Add(900*time.Millisecond)) || record.EndAt.After(after.Add(1100*time.Millisecond)) {
		t.Fatalf("expected connect to extend deadline near now+1s, got %s", record.EndAt)
	}
}

func TestLifecycleExtensionsCannotReviveExpiredSandbox(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
	}{
		{name: "connect", path: "/connect", body: `{"timeout":30}`},
		{name: "timeout", path: "/timeout", body: `{"timeout":30}`},
		{name: "refresh", path: "/refreshes", body: `{"duration":30}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, runtime := newDeadlineTestApp(t)
			created := createSandboxForTest(t, app, "base", http.StatusCreated)
			expiredEndAt := time.Now().UTC().Add(-time.Second)

			// 故意不重排 timer，模拟 EndAt 已到但旧 timer callback 尚未取得
			// lifecycleMu 的窗口。请求路径必须自行识别过期状态。
			setSandboxEndAtWithoutReschedulingForTest(t, app, created.SandboxID, expiredEndAt)

			req := httptest.NewRequest(
				http.MethodPost,
				"/sandboxes/"+created.SandboxID+tt.path,
				bytes.NewBufferString(tt.body),
			)
			rec := httptest.NewRecorder()
			app.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("expected expired sandbox status %d, got %d: %s", http.StatusNotFound, rec.Code, rec.Body.String())
			}
			if _, exists := app.store.Get(created.SandboxID); exists {
				t.Fatal("expected expired sandbox to be removed instead of extended")
			}
			select {
			case deleted := <-runtime.deleted:
				if deleted.ContainerID != "ctr-"+created.SandboxID {
					t.Fatalf("expected runtime deletion for %q, got %#v", created.SandboxID, deleted)
				}
			default:
				t.Fatal("expected expired sandbox runtime to be deleted")
			}
		})
	}
}

func TestExpiredSandboxCannotBeRevivedWhenCleanupFails(t *testing.T) {
	runtime := &recordingRuntime{deleteSandboxErr: context.DeadlineExceeded}
	handler, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	app := handler.(*App)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := app.Shutdown(ctx); err != nil {
			t.Errorf("shutdown app: %v", err)
		}
	})

	created := createSandboxForTest(t, app, "base", http.StatusCreated)
	expiredEndAt := time.Now().UTC().Add(-time.Second)
	setSandboxEndAtWithoutReschedulingForTest(t, app, created.SandboxID, expiredEndAt)

	req := httptest.NewRequest(
		http.MethodPost,
		"/sandboxes/"+created.SandboxID+"/timeout",
		bytes.NewBufferString(`{"timeout":30}`),
	)
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected expired sandbox status %d, got %d: %s", http.StatusNotFound, rec.Code, rec.Body.String())
	}
	record, exists := app.store.Get(created.SandboxID)
	if !exists {
		t.Fatal("expected record to remain while runtime cleanup is retried")
	}
	if !record.EndAt.Equal(expiredEndAt) {
		t.Fatalf("expected failed cleanup to preserve expired EndAt %s, got %s", expiredEndAt, record.EndAt)
	}
}

func TestLifecycleExtensionsRejectMissingRuntimeSandbox(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
	}{
		{name: "connect", path: "/connect", body: `{"timeout":30}`},
		{name: "timeout", path: "/timeout", body: `{"timeout":30}`},
		{name: "refresh", path: "/refreshes", body: `{"duration":30}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, runtime := newDeadlineTestApp(t)
			created := createSandboxForTest(t, app, "base", http.StatusCreated)
			runtime.inspectResults = map[string]SandboxRuntimeInspection{
				"ctr-" + created.SandboxID: {Exists: false},
			}

			req := httptest.NewRequest(
				http.MethodPost,
				"/sandboxes/"+created.SandboxID+tt.path,
				bytes.NewBufferString(tt.body),
			)
			rec := httptest.NewRecorder()
			app.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("expected missing runtime sandbox status %d, got %d: %s", http.StatusNotFound, rec.Code, rec.Body.String())
			}
			if len(runtime.inspectCalls) != 1 {
				t.Fatalf("expected one runtime inspection, got %d", len(runtime.inspectCalls))
			}
			if _, exists := app.store.Get(created.SandboxID); exists {
				t.Fatal("expected missing runtime sandbox mapping to be removed")
			}
		})
	}
}

func TestConnectReconcilesRuntimePausedStateBeforeExtending(t *testing.T) {
	app, runtime := newDeadlineTestApp(t)
	created := createSandboxForTest(t, app, "base", http.StatusCreated)
	runtime.inspectResults = map[string]SandboxRuntimeInspection{
		"ctr-" + created.SandboxID: {
			Exists: true,
			State:  string(e2bapi.Paused),
		},
	}

	req := httptest.NewRequest(
		http.MethodPost,
		"/sandboxes/"+created.SandboxID+"/connect",
		bytes.NewBufferString(`{"timeout":30}`),
	)
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected reconciled paused sandbox status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	if len(runtime.resumeInfos) != 1 {
		t.Fatalf("expected connect to resume runtime-paused sandbox, got %d resume calls", len(runtime.resumeInfos))
	}
	record, exists := app.store.Get(created.SandboxID)
	if !exists || record.State != string(e2bapi.Running) {
		t.Fatalf("expected resumed running record, exists=%t record=%#v", exists, record)
	}
}

func TestConnectDoesNotShortenRunningSandboxDeadline(t *testing.T) {
	app, _ := newDeadlineTestApp(t)
	created := createSandboxForTest(t, app, "base", http.StatusCreated)

	existingEndAt := time.Now().UTC().Add(30 * time.Second)
	setSandboxEndAtForTest(t, app, created.SandboxID, existingEndAt)

	req := httptest.NewRequest(
		http.MethodPost,
		"/sandboxes/"+created.SandboxID+"/connect",
		bytes.NewBufferString(`{"timeout":0}`),
	)
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected connect status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	record, exists := app.store.Get(created.SandboxID)
	if !exists {
		t.Fatal("expected connected sandbox in store")
	}
	if !record.EndAt.Equal(existingEndAt) {
		t.Fatalf("expected connect timeout 0 not to shorten deadline %s, got %s", existingEndAt, record.EndAt)
	}
}

func TestConnectDoesNotShortenPausedSandboxDeadline(t *testing.T) {
	app, _ := newDeadlineTestApp(t)
	created := createSandboxForTest(t, app, "base", http.StatusCreated)

	pauseReq := httptest.NewRequest(http.MethodPost, "/sandboxes/"+created.SandboxID+"/pause", nil)
	pauseRec := httptest.NewRecorder()
	app.ServeHTTP(pauseRec, pauseReq)
	if pauseRec.Code != http.StatusNoContent {
		t.Fatalf("expected pause status %d, got %d: %s", http.StatusNoContent, pauseRec.Code, pauseRec.Body.String())
	}

	existingEndAt := time.Now().UTC().Add(30 * time.Second)
	setSandboxEndAtForTest(t, app, created.SandboxID, existingEndAt)

	connectReq := httptest.NewRequest(
		http.MethodPost,
		"/sandboxes/"+created.SandboxID+"/connect",
		bytes.NewBufferString(`{"timeout":1}`),
	)
	connectRec := httptest.NewRecorder()
	app.ServeHTTP(connectRec, connectReq)
	if connectRec.Code != http.StatusCreated {
		t.Fatalf("expected connect status %d, got %d: %s", http.StatusCreated, connectRec.Code, connectRec.Body.String())
	}

	record, exists := app.store.Get(created.SandboxID)
	if !exists {
		t.Fatal("expected resumed sandbox in store")
	}
	if !record.EndAt.Equal(existingEndAt) {
		t.Fatalf("expected paused connect not to shorten deadline %s, got %s", existingEndAt, record.EndAt)
	}
}

func TestPauseCancelsDeadlineAndResumeStartsNewCycle(t *testing.T) {
	app, runtime := newDeadlineTestApp(t)
	created := createSandboxForTest(t, app, "base", http.StatusCreated)

	pauseReq := httptest.NewRequest(http.MethodPost, "/sandboxes/"+created.SandboxID+"/pause", nil)
	pauseRec := httptest.NewRecorder()
	app.ServeHTTP(pauseRec, pauseReq)
	if pauseRec.Code != http.StatusNoContent {
		t.Fatalf("expected pause status %d, got %d: %s", http.StatusNoContent, pauseRec.Code, pauseRec.Body.String())
	}

	setSandboxEndAtForTest(t, app, created.SandboxID, time.Now().UTC().Add(-time.Second))
	select {
	case deleted := <-runtime.deleted:
		t.Fatalf("paused sandbox should not expire: %#v", deleted)
	case <-time.After(60 * time.Millisecond):
	}

	resumeReq := httptest.NewRequest(
		http.MethodPost,
		"/sandboxes/"+created.SandboxID+"/resume",
		bytes.NewBufferString(`{}`),
	)
	resumeRec := httptest.NewRecorder()
	before := time.Now().UTC()
	app.ServeHTTP(resumeRec, resumeReq)
	after := time.Now().UTC()
	if resumeRec.Code != http.StatusCreated {
		t.Fatalf("expected resume status %d, got %d: %s", http.StatusCreated, resumeRec.Code, resumeRec.Body.String())
	}

	var resumed SandboxResponse
	if err := json.Unmarshal(resumeRec.Body.Bytes(), &resumed); err != nil {
		t.Fatalf("decode resume response: %v", err)
	}
	record, exists := app.store.Get(resumed.SandboxID)
	if !exists {
		t.Fatal("expected resumed sandbox in store")
	}
	minEndAt := before.Add(time.Duration(DefaultSandboxTimeoutSeconds-1) * time.Second)
	maxEndAt := after.Add(time.Duration(DefaultSandboxTimeoutSeconds+1) * time.Second)
	if record.EndAt.Before(minEndAt) || record.EndAt.After(maxEndAt) {
		t.Fatalf("expected resume to start the API default timeout cycle, got %s", record.EndAt)
	}
}

func TestRequestTimeoutDistinguishesOmittedAndZero(t *testing.T) {
	if got := requestTimeout(nil); got != DefaultSandboxTimeoutSeconds {
		t.Fatalf("expected omitted timeout to use API default %d, got %d", DefaultSandboxTimeoutSeconds, got)
	}

	zero := int32(0)
	if got := requestTimeout(&zero); got != 0 {
		t.Fatalf("expected explicit zero timeout to remain zero, got %d", got)
	}

	custom := int32(42)
	if got := requestTimeout(&custom); got != custom {
		t.Fatalf("expected explicit timeout %d, got %d", custom, got)
	}
}

func TestSlowExpiryIODoesNotBlockStoreOrOtherSandboxes(t *testing.T) {
	runtime := &blockingDeleteRuntime{
		deleteStarted: make(chan struct{}),
		releaseDelete: make(chan struct{}),
	}
	handler, err := NewAppWithRuntime(DefaultConfig(), log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	app := handler.(*App)
	t.Cleanup(func() {
		select {
		case <-runtime.releaseDelete:
		default:
			close(runtime.releaseDelete)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := app.Shutdown(ctx); err != nil {
			t.Errorf("shutdown app: %v", err)
		}
	})

	expiring := createSandboxForTest(t, app, "base", http.StatusCreated)
	other := createSandboxForTest(t, app, "base", http.StatusCreated)
	runtime.blockContainerID = "ctr-" + expiring.SandboxID

	setSandboxEndAtForTest(t, app, expiring.SandboxID, time.Now().UTC().Add(20*time.Millisecond))
	select {
	case <-runtime.deleteStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for slow expiry delete")
	}

	start := time.Now()
	otherReq := httptest.NewRequest(
		http.MethodPost,
		"/sandboxes/"+other.SandboxID+"/timeout",
		bytes.NewBufferString(`{"timeout":1}`),
	)
	otherRec := httptest.NewRecorder()
	app.ServeHTTP(otherRec, otherReq)
	if otherRec.Code != http.StatusNoContent {
		t.Fatalf("expected unrelated timeout status %d, got %d: %s", http.StatusNoContent, otherRec.Code, otherRec.Body.String())
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("unrelated sandbox was blocked by expiry I/O for %s", elapsed)
	}

	sameReq := httptest.NewRequest(
		http.MethodPost,
		"/sandboxes/"+expiring.SandboxID+"/timeout",
		bytes.NewBufferString(`{"timeout":1}`),
	)
	sameRec := httptest.NewRecorder()
	sameDone := make(chan struct{})
	go func() {
		app.ServeHTTP(sameRec, sameReq)
		close(sameDone)
	}()

	select {
	case <-sameDone:
		t.Fatal("same-sandbox timeout should wait for expiry I/O")
	case <-time.After(100 * time.Millisecond):
	}

	close(runtime.releaseDelete)
	select {
	case <-sameDone:
	case <-time.After(time.Second):
		t.Fatal("same-sandbox timeout did not resume after expiry I/O")
	}
	if sameRec.Code != http.StatusNotFound {
		t.Fatalf("expected timeout to observe deleted sandbox with status %d, got %d: %s", http.StatusNotFound, sameRec.Code, sameRec.Body.String())
	}
}

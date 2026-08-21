package gateway

import (
	"context"
	"sync"
	"time"

	"e2b-local/internal/e2bapi"
)

const (
	sandboxExpiryRetryDelay    = 5 * time.Second
	sandboxExpiryActionTimeout = 30 * time.Second
)

// sandboxDeadlineManager owns the runtime timers used to actively expire
// sandboxes. Store EndAt values remain the source of truth; timers are rebuilt
// from the store when the gateway starts.
type sandboxDeadlineManager struct {
	mu      sync.Mutex
	timers  map[string]sandboxDeadline
	stopped bool
	wg      sync.WaitGroup
}

type sandboxDeadline struct {
	timer  *time.Timer
	fireAt time.Time
}

func newSandboxDeadlineManager() *sandboxDeadlineManager {
	return &sandboxDeadlineManager{
		timers: make(map[string]sandboxDeadline),
	}
}

// schedule 创建或更新内存定时器，使沙箱在 fireAt 到达时进入过期处理流程。
//
// manager 开始关闭后，该函数不再执行任何操作。如果同一沙箱已经存在触发时间
// 相同的定时器，则保留原定时器，避免无意义的替换。如果触发时间发生变化，则
// 停止旧定时器并保存新定时器。如果 fireAt 已经过期，则使用零延迟调度，让过期
// 处理尽快执行。
//
// manager 的互斥锁只保护 stopped 状态和定时器表，执行 expire 前会释放该锁。
// 回调确认 shutdown 尚未开始后才加入 wg，因此 shutdown 可以等待正在执行的
// 回调结束，同时阻止新的过期任务启动。停止旧定时器无法撤回已经开始的回调，
// 所以 expire 必须在删除前重新检查沙箱当前的 EndAt；调用方因此会捕获
// expectedEndAt，用它识别已经失效的旧回调。
//
// 该函数没有返回值，也不会返回错误。调度成功时会更新定时器表，并可能在稍后
// 调用 expire；shutdown 后调用则直接返回。
//
// 示例：
//   - 将 sbx1 调度到 12:00，会创建一个在 12:00 调用 expire 的定时器。
//   - 将 sbx1 从 12:00 改到 12:05，会停止旧定时器并用新定时器替换它。
//   - shutdown 后再次调度，或重复调度相同的 12:05，不会改变任何状态。
func (m *sandboxDeadlineManager) schedule(sandboxID string, fireAt time.Time, expire func()) {
	// schedule、cancel 和 shutdown 都会访问 timers 和 stopped，
	// 因此必须在同一把锁下完成检查和更新。
	m.mu.Lock()
	defer m.mu.Unlock()

	// shutdown 一旦开始就不再接收新任务，避免关闭过程中又产生定时器。
	if m.stopped {
		return
	}
	if deadline, exists := m.timers[sandboxID]; exists {
		// 截止时间没有变化时复用原定时器，避免检查状态时反复重建 timer。
		if deadline.fireAt.Equal(fireAt) {
			return
		}
		// 截止时间已经变化，停止旧定时器。即使旧回调已经启动，
		// expire 也会通过 expectedEndAt 的二次校验拒绝过期结果。
		deadline.timer.Stop()
	}

	delay := time.Until(fireAt)
	if delay < 0 {
		// 对已经到期的沙箱立即安排处理，而不是创建负延迟定时器。
		delay = 0
	}
	timer := time.AfterFunc(delay, func() {
		// 在加入 WaitGroup 前与 shutdown 串行，保证 shutdown 不会在
		// Wait 返回后又看到一个新启动的回调。
		m.mu.Lock()
		if m.stopped {
			m.mu.Unlock()
			return
		}
		m.wg.Add(1)
		m.mu.Unlock()

		// expire 可能执行较慢的 runtime 删除，不能占用 manager 锁，
		// 否则其他沙箱无法更新或取消自己的定时器。
		defer m.wg.Done()
		expire()
	})
	// 保存当前有效的触发时间，后续调用据此判断是复用还是替换 timer。
	m.timers[sandboxID] = sandboxDeadline{
		timer:  timer,
		fireAt: fireAt,
	}
}

func (m *sandboxDeadlineManager) cancel(sandboxID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if deadline, exists := m.timers[sandboxID]; exists {
		deadline.timer.Stop()
		delete(m.timers, sandboxID)
	}
}

// shutdown 停止所有尚未执行的过期定时器，并等待已经开始的过期回调结束。
//
// 它先在 manager 锁内将 stopped 设为 true，使 schedule 不再接受新任务，定时器
// 回调也不能再加入 wg。随后停止并移除当前保存的所有 timer。Timer.Stop 不能
// 中断已经开始执行的回调，所以函数会在锁外等待 wg 归零。
//
// 等待期间不会持有 manager 锁，正在执行的回调可以正常完成。ctx 只限制等待
// 时间，不会取消 expire 回调本身。所有在途回调完成时返回 nil；如果 ctx 先结束，
// 则返回 ctx.Err()。无论返回哪种结果，manager 都会保持 stopped，不能重新启用。
// 多次调用是安全的，后续调用不会重复停止 timer，只会等待剩余回调。
//
// 示例：
//   - 没有回调运行时，shutdown 停止尚未触发的 timer 并立即返回 nil。
//   - expire 正在删除沙箱时，shutdown 会等到删除完成、wg 归零后返回 nil。
//   - expire 长时间未完成且 ctx 超时时，shutdown 返回 context deadline exceeded，
//     但该回调仍会继续运行，直到自身完成或它使用的 context 被取消。
func (m *sandboxDeadlineManager) shutdown(ctx context.Context) error {
	// 与 schedule 的 stopped 检查和 wg.Add 串行，确保开始等待后不会再有
	// 新回调加入 WaitGroup。
	m.mu.Lock()
	if !m.stopped {
		m.stopped = true
		// 尚未启动的过期任务无需等待，直接停止 timer 并清空索引。
		// 已经开始的回调可能无法被 Stop 撤回，它们由 wg 负责跟踪。
		for sandboxID, deadline := range m.timers {
			deadline.timer.Stop()
			delete(m.timers, sandboxID)
		}
	}
	m.mu.Unlock()

	// WaitGroup 没有支持 context 的等待方法，因此使用单独的 goroutine
	// 将等待完成转换成 channel 信号。
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()

	// 正常情况下等待所有在途回调；调用方也可以通过 ctx 限制关闭耗时。
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// syncSandboxDeadline 根据沙箱当前状态和 EndAt，同步它对应的内存过期定时器。
//
// 该函数把 SandboxRecord 当作一次状态快照。ID 为空时无法定位定时器，因此直接
// 返回。只有 running 且 EndAt 非零的沙箱需要主动过期；其他状态会取消已有
// timer，例如 pause 后不应继续按原截止时间删除沙箱。满足调度条件时，它使用
// EndAt 创建或更新 timer。
//
// 传给回调的 expectedEndAt 是本次快照中的截止时间。即使旧 timer 在被替换时
// 已经开始执行，expireSandbox 仍会将 expectedEndAt 与 Store 中的最新 EndAt
// 比较，从而避免旧回调删除已经续期的沙箱。这样 timer 只负责及时唤醒处理流程，
// Store 中的记录仍是判断是否真正过期的依据。
//
// 该函数没有返回值，也不会修改 SandboxRecord 或返回错误。它的副作用仅限于
// 创建、复用、替换或取消 manager 中的 timer。deadline manager 会用自己的锁
// 保护定时器表，因此可以并发调用；同一沙箱的状态变更通常还由 lifecycleMu
// 串行，确保写入 Store 后再同步对应 timer。
//
// 示例：
//   - running 的 sbx1 EndAt 为 12:00，会创建或保留一个 12:00 触发的 timer。
//   - sbx1 从 running 变为 paused 后再次同步，会取消原有 timer。
//   - sbx1 从 12:00 续期到 12:05 后再次同步，会用 12:05 的 timer 替换旧 timer；
//     即使旧回调已经启动，也会因 expectedEndAt 不匹配而放弃删除。
func (a *App) syncSandboxDeadline(record SandboxRecord) {
	// 空 ID 既不能作为 timer 索引，也无法在过期时查询 Store。
	if record.ID == "" {
		return
	}

	// 只有正在运行且具有明确截止时间的沙箱需要主动过期。
	// pause、删除前状态或缺失 EndAt 时，应撤销之前可能存在的 timer。
	if record.State != string(e2bapi.Running) || record.EndAt.IsZero() {
		a.deadlines.cancel(record.ID)
		return
	}

	// 捕获本次同步的 EndAt。回调执行时会用它校验 Store 中的最新值，
	// 防止已经失效的旧 timer 误删续期后的沙箱。
	expectedEndAt := record.EndAt
	a.deadlines.schedule(record.ID, expectedEndAt, func() {
		a.expireSandbox(record.ID, expectedEndAt)
	})
}

func (a *App) expireSandbox(sandboxID string, expectedEndAt time.Time) {
	entry, exists := a.store.lockSandbox(sandboxID)
	if !exists {
		a.deadlines.cancel(sandboxID)
		return
	}
	defer entry.lifecycleMu.Unlock()

	a.expireSandboxLocked(sandboxID, expectedEndAt)
}

// expireSandboxLocked 在生命周期锁保护下校验沙箱是否仍然过期，并执行配置的过期动作。
//
// 调用方必须已经持有该沙箱的 lifecycleMu；本函数不会自行加锁。锁会覆盖状态
// 检查、runtime pause/delete 和 Store 更新，防止同一沙箱在过期过程中被 pause、
// 续期或重复处理。其他沙箱使用各自的 lifecycleMu，因此不会被这里的慢速 I/O 阻塞。
//
// 函数先读取 Store 中的最新记录。记录不存在时会取消残留 timer。只有状态仍为
// running、当前 EndAt 与 expectedEndAt 完全相同，并且当前时间已经到达 EndAt，
// 才会继续。expectedEndAt 用来识别续期前启动的旧回调；状态和时间检查则
// 避免处理已经暂停或尚未真正到期的沙箱。
//
// runtime 过期动作使用独立的 30 秒超时，不受原请求 context 影响。pause-on-timeout
// 沙箱会暂停 runtime 并把 Store 状态设为 paused；kill-on-timeout 沙箱沿用 required 删除策略。
// runtime 操作失败时保留 Store 记录，记录日志，并在 5 秒后重试。
//
// 该函数没有返回值。主要副作用是暂停或删除 runtime 沙箱、更新 Store、取消 timer，
// 或在失败时写日志并安排重试。错误不会向调用方返回。
//
// 示例：
//   - sbx1 仍为 running，EndAt 等于 12:00，当前时间为 12:01：删除 runtime
//     沙箱和 Store 记录，并取消 timer。
//   - sbx1 已从 12:00 续期到 12:05，但旧回调携带 12:00：EndAt 不匹配，直接
//     返回，不会误删续期后的沙箱。
//   - sbx1 已到期，但 runtime 删除超时或报错：保留 Store 记录，并在 5 秒后
//     使用原 expectedEndAt 再次检查和尝试删除。
func (a *App) expireSandboxLocked(sandboxID string, expectedEndAt time.Time) {
	// lifecycleMu 保证读取后的记录在本次删除流程结束前不会被同沙箱操作修改。
	record, exists := a.store.Get(sandboxID)
	if !exists {
		// Store 已无记录时，清理可能残留的内存 timer 即可。
		a.deadlines.cancel(sandboxID)
		return
	}
	// 三项条件共同确认这是当前有效且确实到期的 running 沙箱。
	// 任一条件不满足都说明该回调已经失效或触发过早。
	if record.State != string(e2bapi.Running) ||
		!record.EndAt.Equal(expectedEndAt) ||
		time.Now().UTC().Before(record.EndAt) {
		return
	}

	// 使用独立且有上限的 context，避免 runtime 删除无限占用 lifecycleMu。
	ctx, cancel := context.WithTimeout(context.Background(), sandboxExpiryActionTimeout)
	defer cancel()

	switch record.OnTimeout {
	case SandboxTimeoutActionPause:
		_, ok, err := a.pauseSandboxLocked(ctx, record)
		if err != nil {
			a.logger.Printf("sandbox expiry pause failed sandbox_id=%s error=%v", sandboxID, err)
			if inspector, supported := a.runtime.(SandboxRuntimeInspector); supported {
				updated, exists, reconcileErr := a.reconcileSandboxRecordWithInspector(ctx, inspector, record)
				switch {
				case reconcileErr != nil:
					a.logger.Printf("sandbox expiry pause reconcile failed sandbox_id=%s error=%v", sandboxID, reconcileErr)
				case !exists:
					return
				case updated.State == string(e2bapi.Paused):
					a.logger.Printf("sandbox expiry pause converged sandbox_id=%s action=pause", sandboxID)
					return
				}
			}
			a.scheduleExpiryRetry(sandboxID, expectedEndAt)
			return
		}
		if !ok {
			a.deadlines.cancel(sandboxID)
			return
		}
		a.logger.Printf("sandbox expired sandbox_id=%s action=pause container_id=%s", sandboxID, record.RuntimeInfo.ContainerID)
		return
	case SandboxTimeoutActionKill:
		// Continue with the required delete transition below.
	default:
		a.logger.Printf("sandbox expiry rejected invalid timeout action sandbox_id=%s action=%q", sandboxID, record.OnTimeout)
		a.deadlines.cancel(sandboxID)
		return
	}

	// required 策略要求 runtime 删除成功后才能移除 Store 记录。
	result, err := a.deleteSandbox(ctx, record, sandboxRuntimeDeleteRequired)
	if err != nil {
		a.logger.Printf("sandbox expiry delete failed sandbox_id=%s error=%v", sandboxID, err)
		// 保留原 expectedEndAt，使重试仍能识别期间发生的 pause 或续期。
		// 例如沙箱在删除超时期间被续期到更晚时间，重试时的二次校验会拒绝删除。
		a.scheduleExpiryRetry(sandboxID, expectedEndAt)
		return
	}
	if !result.Deleted {
		// deleteSandbox 返回 Deleted=false 但 err==nil，说明 Store 记录已经不存在
		// （可能被并发流程删除），无需再记录成功日志。
		return
	}

	a.logger.Printf("sandbox expired sandbox_id=%s container_id=%s", sandboxID, record.RuntimeInfo.ContainerID)
}

func (a *App) scheduleExpiryRetry(sandboxID string, expectedEndAt time.Time) {
	if _, ok := a.store.Get(sandboxID); !ok {
		a.deadlines.cancel(sandboxID)
		return
	}
	a.deadlines.schedule(sandboxID, time.Now().Add(sandboxExpiryRetryDelay), func() {
		a.expireSandbox(sandboxID, expectedEndAt)
	})
}

func (a *App) restoreSandboxDeadlines() {
	for _, record := range a.store.List() {
		a.syncSandboxDeadline(record)
	}
}

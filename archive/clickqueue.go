package archive

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/iyear/tdl/archive/store"
)

// The click queue turns "press every button" into a paced, restart-safe job
// stream. Realtime delivery still clicks inline when the lane is free, so the
// common case stays instant; the queue only absorbs what the lane and the flood
// guard refuse, and it is the only path that can click history that arrived
// before the target was configured.
const (
	// clickPace is the minimum gap between two clicks inside one chat: 20 per
	// minute is the ceiling the flood guard already encodes, 3s lands on it.
	clickPace = 3 * time.Second
	// clickQueueBatch bounds one pass so a long backlog drains steadily
	// instead of holding the worker for minutes.
	clickQueueBatch = 6
	// clickQueueSweep is how often the worker looks for due jobs.
	clickQueueSweep = 2 * time.Second
	// clickQueueMaxAttempt bounds retries of a message that keeps failing for
	// a reason that is not obviously permanent.
	clickQueueMaxAttempt = 4
)

// StartQueue launches the background click worker. Idempotent: the supervisor
// may reconnect many times, but only one worker ever runs per process.
func (a *AutoClicker) StartQueue(ctx context.Context) {
	a.queueOnce.Do(func() {
		go func() {
			t := time.NewTicker(clickQueueSweep)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					a.QueuePass(ctx)
				}
			}
		}()
	})
}

// QueuePass clicks every due job whose chat is out of its pacing window.
func (a *AutoClicker) QueuePass(ctx context.Context) {
	rt := a.runtime()
	if rt == nil {
		return
	}
	jobs, err := a.Store.DueClicks(ctx, time.Now().Unix(), clickQueueBatch)
	if err != nil {
		slog.Default().Debug("autoclick queue: due", slog.String("error", err.Error()))
		return
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			return
		}
		if !a.paceOK(j.ChatID) {
			// A click in this chat just happened. Leave the job pending rather
			// than rewriting next_at: the next tick picks it up and the backlog
			// keeps its arrival order.
			continue
		}
		runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), autoclickTimeout)
		a.clickQueued(runCtx, rt, j)
		cancel()
	}
}

// paceOK enforces one click per chat per clickPace.
func (a *AutoClicker) paceOK(chatID int64) bool {
	a.lastClickMu.Lock()
	defer a.lastClickMu.Unlock()
	if a.lastClick == nil {
		a.lastClick = map[int64]time.Time{}
	}
	if t, ok := a.lastClick[chatID]; ok && time.Since(t) < clickPace {
		return false
	}
	a.lastClick[chatID] = time.Now()
	return true
}

// clickQueued resolves one queued message against the live server and presses
// it. The live re-read is the whole point: the archive only keeps button
// labels, and a callback needs the payload that arrived with the message.
func (a *AutoClicker) clickQueued(ctx context.Context, rt *Runtime, j store.ClickJob) {
	x, err := a.Store.QueuedMessage(ctx, j.ChatID, j.MessageID)
	if err != nil {
		// Only a missing message is terminal. A canceled context (shutdown) or a
		// busy database used to mark the click "gone" for good, which silently
		// threw away every click that overlapped a shutdown.
		if errors.Is(err, sql.ErrNoRows) {
			_ = a.Store.MarkClick(ctx, j.ChatID, j.MessageID, store.ClickGone, store.CleanClickError(err))
			return
		}
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return
		}
		a.reschedule(ctx, j, "读取消息失败: "+err.Error())
		return
	}
	// Local first: the archived raw JSON already knows every text-link
	// destination. Fetching only when the local copy has nothing to click keeps
	// a 1000-message backfill from turning into 1000 API calls — which is
	// exactly how a flood ban happens.
	btns := clickablesFromRaw(x)
	var viaBot int64
	if len(btns) == 0 {
		peer, perr := ResolveChatID(ctx, rt, j.ChatID)
		if perr != nil {
			a.reschedule(ctx, j, "解析聊天失败: "+perr.Error())
			return
		}
		m, merr := a.liveMessage(ctx, rt, peer, x)
		if merr != nil {
			msg := merr.Error()
			if strings.Contains(msg, "已被删除") || strings.Contains(msg, "已不在会话中") {
				_ = a.Store.MarkClick(ctx, j.ChatID, j.MessageID, store.ClickGone, msg)
				return
			}
			a.reschedule(ctx, j, msg)
			return
		}
		btns = clickables(m)
		viaBot, _ = m.GetViaBotID()
	}
	if t, terr := a.Store.Target(ctx, j.ChatID); terr == nil {
		btns = filterClickables(btns, t.AutoClickKeywords, x)
	}
	if len(btns) == 0 {
		_ = a.Store.MarkClick(ctx, j.ChatID, j.MessageID, store.ClickEmpty, "消息里没有可点的按钮")
		return
	}
	if ok := a.clickAll(ctx, x, btns, viaBot); ok > 0 {
		_ = a.Store.MarkClick(ctx, j.ChatID, j.MessageID, store.ClickDone, "")
		return
	}
	a.reschedule(ctx, j, "本次点击未成功")
}

// reschedule backs off exponentially and gives up after clickQueueMaxAttempt.
func (a *AutoClicker) reschedule(ctx context.Context, j store.ClickJob, reason string) {
	if j.Attempts+1 >= clickQueueMaxAttempt {
		_ = a.Store.MarkClick(ctx, j.ChatID, j.MessageID, store.ClickFailed, reason)
		return
	}
	backoff := time.Duration(1<<uint(j.Attempts)) * 30 * time.Second
	_ = a.Store.RescheduleClick(ctx, j.ChatID, j.MessageID, time.Now().Add(backoff).Unix(), reason)
}

// Enqueue adds message ids to the click queue.
func (a *AutoClicker) Enqueue(ctx context.Context, chatID int64, msgIDs []int) (int, error) {
	return a.Store.EnqueueClicks(ctx, chatID, msgIDs, 0)
}

// ScanChat backfills the click queue for one chat. By default only messages
// whose archived keyboard carried a callback button are enqueued; includeRecent
// widens it to every recent inbound message for archives written before button
// capture existed. Widening costs one message fetch per already-handled
// message, so it is opt-in.
func (a *AutoClicker) ScanChat(ctx context.Context, chatID int64, limit int, includeRecent bool) (int, error) {
	var (
		ids []int
		err error
	)
	if includeRecent {
		ids, err = a.Store.InboundMessageIDs(ctx, chatID, limit)
	} else {
		ids, err = a.Store.ButtonedMessageIDs(ctx, chatID, limit)
	}
	if err != nil {
		return 0, err
	}
	return a.Enqueue(ctx, chatID, ids)
}

package com.tdl.archive.app;

import com.tdl.archive.mobile.Bridge;

import java.util.concurrent.atomic.AtomicInteger;

/**
 * Process-wide handle to the running Go core.
 *
 * <p>The bridge object lives in {@link ArchiveService}, not in the activity:
 * the activity is destroyed on rotation and while backgrounded, and tearing the
 * Telegram connection down with it would defeat the point of archiving. The
 * service owns it; this holder only publishes what the UI needs to know.
 *
 * <p>State changes are generation-guarded: {@link #markStarting()} hands out a
 * token and only the matching {@link #setReady} / {@link #setFailed} /
 * {@link #clear(int)} may publish. Without this, a service replaced mid-boot
 * lets the old boot thread overwrite the new generation's state — the WebView
 * would then load the old core's credentials, the exact situation
 * applyCredentials exists to prevent.
 */
final class CoreHolder {

    private static volatile Bridge bridge;
    private static volatile String addr;
    private static volatile String token;
    private static volatile String error;
    private static volatile boolean starting;
    private static final AtomicInteger generation = new AtomicInteger();

    private CoreHolder() {}

    static synchronized int markStarting() {
        int gen = generation.incrementAndGet();
        // 新 generation 一开始，上一代发布的东西就作废了，必须一起清掉。
        // 只清 starting/error 是不够的：isReady() 只看 addr 非空，服务被系统
        // 单独重建（进程还在、静态字段还有值）时它仍返回 true，pollForCore 会
        // 拿着一个已经死掉的端口去 loadUrl —— 白屏，而且不会走任何错误分支。
        bridge = null;
        addr = null;
        token = null;
        starting = true;
        error = null;
        return gen;
    }

    /** Only the boot holding the current generation may publish. */
    static synchronized void setReady(Bridge b, String address, String authToken, int gen) {
        if (gen != generation.get()) {
            return; // superseded by a newer service instance
        }
        bridge = b;
        addr = address;
        token = authToken;
        error = null;
        starting = false;
    }

    static synchronized void setFailed(String message, int gen) {
        if (gen != generation.get()) {
            return; // a stale boot's failure must not mask the live one
        }
        error = message;
        starting = false;
    }

    /** Only clears when no newer generation has started (service destroy path). */
    static synchronized void clear(int gen) {
        if (gen != generation.get()) {
            return;
        }
        bridge = null;
        addr = null;
        token = null;
        error = null;
        starting = false;
    }

    /** The generation a service instance captured; used for ownership checks. */
    static int currentGeneration() {
        return generation.get();
    }

    static Bridge getBridge() {
        return bridge;
    }

    static String getAddr() {
        return addr;
    }

    /** Per-boot bearer token for the loopback server; empty until ready. */
    static String getToken() {
        return token;
    }

    static String getError() {
        return error;
    }

    static boolean isStarting() {
        return starting;
    }

    static boolean isReady() {
        return addr != null && !addr.isEmpty();
    }

    /**
     * True when the live generation ended in a boot failure.
     *
     * <p>{@link #markStarting()} clears the error, so this can never report a
     * failure for a boot that is currently in flight.
     */
    static synchronized boolean isFailed() {
        return error != null;
    }
}

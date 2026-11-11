package com.tdl.archive.app;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.os.Build;
import android.os.Handler;
import android.os.IBinder;
import android.os.Looper;
import android.os.PowerManager;
import android.util.Log;

import com.tdl.archive.mobile.Bridge;
import com.tdl.archive.mobile.Mobile;

import java.io.File;
import java.io.IOException;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;

/**
 * Hosts the Go core for as long as the app is installed.
 *
 * <p>Why a foreground service rather than a thread in the activity: the archive
 * has to keep the MTProto socket open while the screen is off and while the
 * activity is destroyed. An activity-scoped worker gets frozen within minutes
 * on every modern Android build. This mirrors how messaging apps stay reachable.
 */
public class ArchiveService extends Service {

    private static final String TAG = "ArchiveService";
    private static final String CHANNEL_ID = "archive_core";
    private static final int NOTIFICATION_ID = 1001;

    private PowerManager.WakeLock wakeLock;
    private volatile boolean booted;
    // 本实例创建的 bridge：onDestroy 只关自己的，绝不关后来者发布的
    // （旧实例销毁晚于新实例启动时，getBridge() 可能已经是别人的）。
    private volatile Bridge ownBridge;
    private volatile int myGeneration;
    /** 本实例的销毁握手；见 releaseShutdownLatch 与 shutdownLatch 的注释。 */
    private volatile CountDownLatch myShutdownLatch;

    // Boot self-healing. START_STICKY only resurrects the *service process*;
    // it does not retry a bootCore that failed (Telegram unreachable, a KV
    // lock left by the instance that was just replaced, a transient OOM). The
    // service used to stay alive with a dead core until the user opened the app
    // again, so the archive silently stopped. Failures are now retried on a
    // backoff while the service is alive.
    private static final long BOOT_RETRY_BASE_MS = 60_000L;
    private static final long BOOT_RETRY_MAX_MS = 30L * 60_000L;
    private final Handler retryHandler = new Handler(Looper.getMainLooper());

    // A wake lock without a timeout is held until onDestroy. If that path is
    // ever missed (a crash in a shutdown thread, an OS that reuses the process,
    // a bug), the phone keeps burning battery with nothing to show for it. The
    // lock is therefore taken for 6h and refreshed at 5h: the timeout is a
    // backstop, not the lifetime. The old comment worried that a plain
    // acquire(6h) silently stopped archiving after the sixth hour; refresh
    // removes exactly that failure mode.
    private static final long WAKELOCK_TIMEOUT_MS = 6L * 60 * 60 * 1000;
    private static final long WAKELOCK_REFRESH_MS = 5L * 60 * 60 * 1000;
    private final Handler wakeLockHandler = new Handler(Looper.getMainLooper());
    private volatile boolean destroyed;
    private volatile boolean booting;
    private int bootFailures;

    /**
     * 前一个实例的销毁握手：applyCredentials 等它再重启，取代 700ms 拍脑袋。
     *
     * <p>必须是可替换的引用而不是 static final。CountDownLatch 是一次性的：
     * 写成常量后第一次 countDown 就把计数耗尽，从第二次换凭据起这个握手
     * 永久失效（立刻返回，退化成原始竞态）；反过来若当前根本没有在运行的
     * 服务，await 又必然白等满 2 秒。每次 onCreate 换一个新的。
     */
    private static volatile CountDownLatch shutdownLatch;

    /** 最多等 prevDestroy 这么久；拿不到也只能继续（旧行为兜底）。 */
    static boolean awaitPreviousDestroy(long timeoutMs) {
        CountDownLatch latch = shutdownLatch;
        if (latch == null) {
            // 没有待销毁的旧实例，没什么可等的。
            return true;
        }
        try {
            return latch.await(timeoutMs, TimeUnit.MILLISECONDS);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            return false;
        }
    }

    /**
     * 解除本实例的销毁握手。
     *
     * <p>必须传自己那份 latch，不能去读静态字段再 countDown。旧实例的关闭线程
     * 可能比 2 秒握手窗口还慢（stop()/close() 要抢 Go 侧那把在拨号期间一直被
     * 持有的锁），等它跑完时静态字段里装的已经是**新实例**的 latch：照静态字段
     * countDown 会把新 latch 的计数耗光再置 null，于是从第三次换凭据起
     * awaitPreviousDestroy 直接返回 true，握手退化成原来那个 700ms 拍脑袋的
     * 竞态 —— 正是这个 latch 当初要解决的问题。
     */
    private static void releaseShutdownLatch(CountDownLatch mine) {
        if (mine == null) {
            return;
        }
        mine.countDown();
        synchronized (ArchiveService.class) {
            // 只有本实例的 latch 仍是"当前"那份时才作废它。
            if (shutdownLatch == mine) {
                shutdownLatch = null;
            }
        }
    }

    static void start(Context ctx) {
        Intent i = new Intent(ctx, ArchiveService.class);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            ctx.startForegroundService(i);
        } else {
            ctx.startService(i);
        }
    }

    static void stop(Context ctx) {
        ctx.stopService(new Intent(ctx, ArchiveService.class));
    }

    @Override
    public void onCreate() {
        super.onCreate();
        createChannel();
        startForeground(NOTIFICATION_ID, buildNotification(getString(R.string.starting)));

        PowerManager pm = (PowerManager) getSystemService(POWER_SERVICE);
        if (pm != null) {
            wakeLock = pm.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "tdl:archive-core");
            wakeLock.setReferenceCounted(false);
            // Refresh well before the timeout so the lock never lapses while the
            // service is alive (see WAKELOCK_TIMEOUT_MS). setReferenceCounted
            // (false) makes a repeated acquire replace the timeout instead of
            // incrementing a counter that onDestroy would have to unwind.
            refreshWakeLock();
        }

        // 本实例在销毁时会 countDown 它，让等待方（换凭据后的重启）知道
        // 旧实例已经走完。每次 onCreate 换新，见 shutdownLatch 的注释。
        myShutdownLatch = new CountDownLatch(1);
        shutdownLatch = myShutdownLatch;

        destroyed = false;
        booting = true;
        myGeneration = CoreHolder.markStarting();
        // Boot on a worker thread: newBridge opens the KV store and start()
        // opens SQLite and dials Telegram, all of which block. Doing it on the
        // main thread would ANR before the first frame.
        startBootThread(myGeneration, "tdl-core-boot");
    }

    private void startBootThread(int gen, String name) {
        Thread t = new Thread(() -> bootCore(gen), name);
        t.setDaemon(false);
        t.start();
    }

    /**
     * (Re)takes the partial wake lock and schedules the next refresh.
     *
     * <p>Called on the main thread only: onCreate and the wakeLockHandler
     * callback. A repeated acquire on a non-reference-counted lock replaces the
     * timeout, so the lock never lapses while the service is alive.
     */
    private void refreshWakeLock() {
        if (destroyed || wakeLock == null) {
            return;
        }
        wakeLock.acquire(WAKELOCK_TIMEOUT_MS);
        wakeLockHandler.removeCallbacksAndMessages(null);
        wakeLockHandler.postDelayed(this::refreshWakeLock, WAKELOCK_REFRESH_MS);
    }

    /** Starts a fresh boot unless one is already running or the core is up. */
    private synchronized void retryBootNow() {
        if (destroyed || booted || booting) {
            return;
        }
        booting = true;
        myGeneration = CoreHolder.markStarting();
        startBootThread(myGeneration, "tdl-core-boot-retry");
    }

    /**
     * Schedules the next boot attempt with exponential backoff.
     *
     * <p>The delay is capped so a permanently broken configuration (for
     * example a missing credential) is retried rarely rather than never, and
     * an intermittent one (network at launch, a stale lock) recovers quickly.
     */
    private void scheduleRetry() {
        if (destroyed) {
            return;
        }
        bootFailures++;
        long delay = BOOT_RETRY_BASE_MS;
        for (int i = 1; i < bootFailures && delay < BOOT_RETRY_MAX_MS; i++) {
            delay = Math.min(delay * 2, BOOT_RETRY_MAX_MS);
        }
        Log.w(TAG, "core boot failed; retry #" + bootFailures + " in " + delay + "ms");
        retryHandler.postDelayed(() -> retryBootNow(), delay);
    }

    private void bootCore(int gen) {
        Bridge bridge = null;
        File crashLog = new File(getFilesDir(), "crash.log");
        File mark = new File(getFilesDir(), "boot.log");
        // Pure-Java breadcrumbs. They must not touch the Go bridge: the whole
        // point is to record how far we get before the first JNI call, which is
        // where the process currently dies with SIGABRT.
        jlog(mark, "bootCore entered");
        // Must run before any other bridge call: it wires the Go runtime's
        // crash handler to this file. Without it a native panic aborts the
        // process with nothing recorded, because an app's fd 2 is /dev/null
        // and release ROMs commonly hide logcat entirely.
        try {
            jlog(mark, "calling installCrashLog");
            Mobile.installCrashLog(crashLog.getAbsolutePath());
            jlog(mark, "installCrashLog returned");
        } catch (Throwable t) {
            jlog(mark, "installCrashLog threw: " + t);
            Log.w(TAG, "cannot install crash log", t);
        }
        try {
            int appId = Prefs.getAppId(this);
            String appHash = Prefs.getAppHash(this);
            if (appId <= 0 || appHash.isEmpty()) {
                throw new IllegalStateException("Telegram api_id/api_hash are not configured");
            }

            File dataDir = new File(getFilesDir(), "tdl");
            if (!dataDir.exists() && !dataDir.mkdirs()) {
                throw new IOException("cannot create data dir " + dataDir);
            }

            jlog(mark, "dataDir=" + dataDir.getAbsolutePath()
                    + " appId=" + appId
                    + " hashLen=" + appHash.length());

            jlog(mark, "calling newBridge");
            bridge = Mobile.newBridge(dataDir.getAbsolutePath());
            jlog(mark, "newBridge returned");

            // The licence binds to this physical device. The material is
            // composed in Java because only the app has the Context needed to
            // read ANDROID_ID; the Go core hashes it into the 8-character code
            // the activation screen shows.
            String deviceMaterial = DeviceId.material(this);
            jlog(mark, "deviceMaterial fp=" + DeviceId.fingerprint(this));
            bridge.setDeviceMaterial(deviceMaterial);

            // Port 0 asks the kernel for a free port. A fixed port would collide
            // with anything else on the device and is visible to other apps.
            jlog(mark, "calling start");
            String address = bridge.start(appId, appHash, 0);
            jlog(mark, "start returned " + address);
            // Publishing and onDestroy must agree on whether this instance is
            // still alive. Without the lock a boot that finishes just after
            // onDestroy captured ownBridge (null) would publish a bridge
            // nobody closes: leaked loopback port, bolt lock and MTProto
            // socket, and the next start fails on the flock.
            synchronized (ArchiveService.this) {
                if (destroyed) {
                    try {
                        bridge.stop();
                        bridge.close();
                    } catch (Throwable t) {
                        Log.w(TAG, "error closing late bridge", t);
                    }
                    return;
                }
                booted = true;
                ownBridge = bridge;
                // AuthToken：WebView 每个请求都要带的回环访问凭据（见 bridge.go）
                CoreHolder.setReady(bridge, address, bridge.authToken(), gen);
                bootFailures = 0;
                Log.i(TAG, "core ready on " + address);
                updateNotification(getString(R.string.notif_text));
            }
        } catch (Throwable t) {
            jlog(mark, "boot FAILED: " + Log.getStackTraceString(t));
            Log.e(TAG, "core boot failed", t);
            CoreHolder.setFailed(String.valueOf(t.getMessage()), gen);
            updateNotification(getString(R.string.start_failed) + ": " + t.getMessage());
            if (bridge != null) {
                try {
                    bridge.close();
                } catch (Throwable ignored) {
                    // best effort: the process is in a failed state anyway
                }
            }
            scheduleRetry();
        } finally {
            booting = false;
        }
    }

    /**
     * Appends one breadcrumb line using only the JDK.
     *
     * <p>Deliberately does not call into the Go bridge: this has to work even
     * when the bridge is what kills the process.
     */
    private static void jlog(File f, String line) {
        try (java.io.FileOutputStream fo = new java.io.FileOutputStream(f, true)) {
            fo.write((System.currentTimeMillis() + " " + line + "\n").getBytes("UTF-8"));
            fo.flush();
        } catch (Throwable ignored) {
            // diagnostics must never break the boot path
        }
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        // START_STICKY: if the OS kills us for memory pressure it recreates the
        // service and the archive resumes without the user opening the app.
        //
        // A restart also gives us a chance to recover from an earlier failed
        // boot immediately instead of waiting out the backoff timer.
        if (!booted && !booting && CoreHolder.isFailed()) {
            retryBootNow();
        }
        return START_STICKY;
    }

    @Override
    public void onDestroy() {
        final Bridge b;
        final boolean mine;
        final int gen;
        final CountDownLatch latch;
        // Capture the published state under the same lock bootCore publishes
        // with, and stop retries: a delayed attempt would resurrect a boot
        // whose bridge the shutdown thread is about to close.
        synchronized (this) {
            destroyed = true;
            retryHandler.removeCallbacksAndMessages(null);
            wakeLockHandler.removeCallbacksAndMessages(null);
            b = ownBridge;
            mine = b != null;
            gen = myGeneration;
            latch = myShutdownLatch;
        }
        // 只关本实例自己创建的 bridge：ownBridge 只会在本实例 bootCore 成功时
        // 被赋值，天然是唯一的，关它绝不会碰到后来者的实例。
        //
        // 这里曾经写成 `b == CoreHolder.getBridge()` 来"防止关掉后来者的"，
        // 但那个判断是错的，而且正好把唯一的重启路径堵死：applyCredentials 在
        // 主线程先 stopService（异步）再同步 CoreHolder.clear()，等 onDestroy
        // 真正跑起来时 getBridge() 已经是 null，判断恒为 false，b.stop()/
        // b.close() 被整段跳过。结果是旧 core 的监听端口、bolt 文件锁、MTProto
        // 连接全部留着，新实例 NewBridge 抢同一把 flock —— 通知上那个"更换 API
        // 凭据"按钮等于完全失效。

        // 卸载到后台线程再关。bridge.stop()/close() 要抢 Go 侧的 mutex，而
        // Start() 在拨号期间全程持有同一把锁（SQLite 打开 + MTProto 建连，
        // 慢机器上可达数十秒）。在主线程等这把锁等于把 onDestroy 钉死：系统
        // 几秒后按 ANR 杀掉进程，那把锁反而永远等不到释放，下次启动必失败。
        // 挪到后台线程，主线程立即返回，握手仍在真正关闭完成后才解除。
        Thread closer = new Thread(() -> {
            try {
                if (mine) {
                    try {
                        b.stop();
                        b.close();
                    } catch (Throwable t) {
                        Log.w(TAG, "error closing bridge", t);
                    }
                }
            } finally {
                CoreHolder.clear(gen);
                releaseShutdownLatch(latch); // applyCredentials 的等待在此解除
                if (wakeLock != null && wakeLock.isHeld()) {
                    wakeLock.release();
                }
            }
        }, "tdl-core-shutdown");
        closer.setDaemon(false);
        closer.start();

        super.onDestroy();
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }

    private void createChannel() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) {
            return;
        }
        NotificationManager nm = getSystemService(NotificationManager.class);
        if (nm == null) {
            return;
        }
        NotificationChannel ch = new NotificationChannel(
                CHANNEL_ID,
                getString(R.string.notif_channel_name),
                NotificationManager.IMPORTANCE_LOW);
        ch.setDescription(getString(R.string.notif_channel_desc));
        ch.setShowBadge(false);
        nm.createNotificationChannel(ch);
    }

    private Notification buildNotification(String text) {
        Intent open = new Intent(this, MainActivity.class);
        open.setFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP | Intent.FLAG_ACTIVITY_CLEAR_TOP);
        int flags = PendingIntent.FLAG_UPDATE_CURRENT;
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
            flags |= PendingIntent.FLAG_IMMUTABLE;
        }
        PendingIntent pi = PendingIntent.getActivity(this, 0, open, flags);

        // Escape hatch for the shared compiled-in credentials: a rate-limited
        // install needs a way to enter its own api_id/api_hash without wiping
        // app data. The first-run setup screen never appears once a built-in
        // pair exists, so it has to be reachable from somewhere else.
        Intent reconfigure = new Intent(this, MainActivity.class);
        reconfigure.setFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP | Intent.FLAG_ACTIVITY_CLEAR_TOP);
        reconfigure.putExtra(MainActivity.EXTRA_FORCE_SETUP, true);
        PendingIntent credPi = PendingIntent.getActivity(this, 1, reconfigure, flags);

        Notification.Builder b;
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            b = new Notification.Builder(this, CHANNEL_ID);
        } else {
            b = new Notification.Builder(this);
        }
        return b.setContentTitle(getString(R.string.notif_title))
                .setContentText(text)
                .setSmallIcon(android.R.drawable.stat_notify_sync)
                .setContentIntent(pi)
                .addAction(android.R.drawable.ic_menu_manage,
                        getString(R.string.notif_action_credentials), credPi)
                .setOngoing(true)
                .build();
    }

    private void updateNotification(String text) {
        NotificationManager nm = (NotificationManager) getSystemService(NOTIFICATION_SERVICE);
        if (nm != null) {
            nm.notify(NOTIFICATION_ID, buildNotification(text));
        }
    }
}

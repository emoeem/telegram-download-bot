package com.tdl.archive.app;

import android.Manifest;
import android.app.Activity;
import android.app.AlertDialog;
import android.content.DialogInterface;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.content.res.Configuration;
import android.graphics.Color;
import android.graphics.drawable.GradientDrawable;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.os.StatFs;
import android.provider.Settings;
import android.text.InputType;
import android.util.TypedValue;
import android.view.Gravity;
import android.view.KeyEvent;
import android.view.View;
import android.view.ViewGroup;
import android.view.Window;
import android.view.WindowInsets;
import android.view.WindowInsetsController;
import android.webkit.CookieManager;
import android.webkit.JavascriptInterface;
import android.webkit.JsPromptResult;
import android.webkit.JsResult;
import android.webkit.MimeTypeMap;
import android.webkit.WebChromeClient;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.Button;
import android.widget.EditText;
import android.widget.FrameLayout;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.TextView;
import android.widget.Toast;

import java.io.File;
import java.io.FileOutputStream;
import java.io.FileWriter;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.io.PrintWriter;
import java.net.HttpURLConnection;
import java.net.URL;
import java.util.Arrays;
import java.util.Date;
import java.util.HashMap;
import java.util.Locale;
import java.util.Map;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * Single-activity shell: collects credentials on first run, then shows the
 * archive UI served by the in-process Go core.
 *
 * <p>The UI is built in code rather than XML because there are exactly two
 * screens and neither needs resources beyond strings.
 */
public class MainActivity extends Activity {

    private static final String CORE_URL_PREFIX = "http://";
    private static final long WAIT_TIMEOUT_MS = 60_000L;
    private static final long POLL_INTERVAL_MS = 250L;
    private static final int REQ_NOTIFICATIONS = 41;

    /**
     * 导出/媒体下载的上限。回环服务是自己人，但文件来自 Telegram 云端，
     * 一个异常大的导出（或恶意账号）足以把手机存储写满，进而让归档本身
     * 无法写库。超过上限直接在流里中止，不落盘。
     */
    private static final long MAX_DOWNLOAD_BYTES = 512L * 1024 * 1024;
    /** 写文件前至少保留的空间，避免把系统/应用自己的缓存挤爆。 */
    private static final long MIN_FREE_BYTES = 32L * 1024 * 1024;
    /** cacheDir/shared 的总占用上限，超出按最旧优先清理。 */
    private static final long MAX_SHARED_CACHE_BYTES = 1024L * 1024 * 1024;
    /** 缓存文件保留时长，过期在下次下载前清掉。 */
    private static final long SHARED_CACHE_TTL_MS = 24L * 60 * 60 * 1000;

    /**
     * 从 Content-Disposition 里抠文件名，`filename="x"` 与
     * `filename*=UTF-8''x` 两种写法都要认。
     */
    private static final Pattern CD_FILENAME = Pattern.compile(
            "filename\\*?=(?:UTF-8'')?\"?([^\";]+)\"?", Pattern.CASE_INSENSITIVE);

    /** 保证 installJavaCrashLog 进程内只执行一次（见该方法）。 */
    private static volatile boolean crashLogInstalled;

    /**
     * Forces the credential screen even though a compiled-in pair exists.
     *
     * <p>Reached from the ongoing notification's action: the shared pair works
     * out of the box, but an install that hits rate limits must be able to
     * substitute its own without clearing app data.
     */
    static final String EXTRA_FORCE_SETUP = "force_setup";

    private final Handler handler = new Handler(Looper.getMainLooper());

    private WebView web;
    private TextView status;
    private long waitStartedAt;
    private boolean loaded;
    // 凭据页是直接 setContentView 换掉的，WebView 仍活着但被摘下来。
    // 记住归档页的根视图，返回键就能原样贴回去，不用重新加载丢掉当前视图。
    private View archiveRoot;
    private boolean setupVisible;
    // 系统栏/输入法 insets 作用的容器；键盘收起后主动重算一次，
    // 避免上一次的输入法高度把页面永久压矮。
    private View insetShell;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        installJavaCrashLog();
        requestNotificationPermission();

        if (!wantsSetup() && Prefs.hasCredentials(this)) {
            showArchive();
        } else {
            showSetup(null);
        }
    }

    /**
     * Applies the system bar icon appearance once the window is attached.
     *
     * <p>Called from onPostResume rather than onCreate: Window.getInsetsController
     * is only meaningful after the decor view exists, and releasing a failure
     * here into onCreate turned an optional cosmetic touch into an app that
     * could not start at all.
     */
    @Override
    protected void onPostResume() {
        super.onPostResume();
        try {
            applyDarkSystemBarIcons();
        } catch (Throwable error) {
            // Cosmetic only; never worth blocking the archive behind.
            logSuppressed("applyDarkSystemBarIcons", error);
        }
    }

    @Override
    protected void onNewIntent(Intent intent) {
        super.onNewIntent(intent);
        // SINGLE_TOP reuses this instance, so the notification action arrives
        // here rather than in onCreate.
        setIntent(intent);
        if (wantsSetup()) {
            showSetup(null);
        }
    }

    private boolean wantsSetup() {
        Intent intent = getIntent();
        return intent != null && intent.getBooleanExtra(EXTRA_FORCE_SETUP, false);
    }

    // ---------------------------------------------------------------- setup

    private void showSetup(String problem) {
        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setBackgroundColor(Color.parseColor("#080a0f"));
        int pad = dp(20);
        // The status bar is reserved by wrapWithSystemBarInsets, so this only
        // has to add breathing room rather than guess the bar height.
        root.setPadding(pad, dp(20), pad, pad);

        TextView title = new TextView(this);
        // 这个页面既是首次配置，也是「更换 Telegram 凭据」的落地页；
        // 写「首次配置」会让已登录用户以为自己的账号被重置了。
        title.setText("Telegram API 凭据");
        title.setTextColor(Color.parseColor("#e8eef7"));
        title.setTextSize(TypedValue.COMPLEX_UNIT_SP, 22);
        root.addView(title);

        TextView help = new TextView(this);
        help.setText("归档核心需要一对 Telegram API 凭据（api_id / api_hash）。\n\n"
                + "在浏览器打开 my.telegram.org → 登录 → API development tools，"
                + "应用名随意填（例如 tdl），Platform 选 Other，页面会给出这两个值。\n\n"
                + "它们标识的是这个应用，不是你的账号；登录会话另行通过扫码建立，"
                + "只保存在本机应用私有目录。");
        help.setTextColor(Color.parseColor("#8fa3bf"));
        help.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        help.setPadding(0, dp(12), 0, dp(20));
        root.addView(help);

        if (problem != null) {
            TextView err = new TextView(this);
            err.setText(problem);
            err.setTextColor(Color.parseColor("#ff6b6b"));
            err.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
            err.setPadding(0, 0, 0, dp(12));
            root.addView(err);
        }

        final EditText idInput = new EditText(this);
        idInput.setHint("api_id（纯数字）");
        idInput.setInputType(InputType.TYPE_CLASS_NUMBER);
        idInput.setTextColor(Color.parseColor("#e8eef7"));
        idInput.setHintTextColor(Color.parseColor("#5a6b80"));
        styleSetupField(idInput);
        root.addView(idInput);

        final EditText hashInput = new EditText(this);
        hashInput.setHint("api_hash（32 位十六进制）");
        hashInput.setInputType(InputType.TYPE_CLASS_TEXT);
        hashInput.setTextColor(Color.parseColor("#e8eef7"));
        hashInput.setHintTextColor(Color.parseColor("#5a6b80"));
        styleSetupField(hashInput);
        root.addView(hashInput);

        Button save = new Button(this);
        save.setText("保存并启动");
        styleSetupButton(save, true);
        save.setOnClickListener(v -> {
            String idText = idInput.getText().toString().trim();
            String hash = hashInput.getText().toString().trim();
            int appId;
            try {
                appId = Integer.parseInt(idText);
            } catch (NumberFormatException e) {
                showSetup("api_id 必须是数字");
                return;
            }
            if (appId <= 0 || hash.isEmpty()) {
                showSetup("api_id 和 api_hash 都要填");
                return;
            }
            Prefs.setCredentials(MainActivity.this, appId, hash);
            applyCredentials();
        });
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.topMargin = dp(20);
        root.addView(save, lp);

        // The compiled-in pair already works, so this screen is only ever an
        // opt-out. Give it a way back that does not require retyping anything.
        if (Prefs.hasCredentials(this)) {
            Button useBuiltin = new Button(this);
            useBuiltin.setText("沿用内置凭据");
            styleSetupButton(useBuiltin, false);
            useBuiltin.setOnClickListener(v -> {
                Prefs.clearCredentials(MainActivity.this);
                applyCredentials();
            });
            LinearLayout.LayoutParams lp2 = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            lp2.topMargin = dp(8);
            root.addView(useBuiltin, lp2);
        }

        ScrollView scroll = new ScrollView(this);
        scroll.addView(root);
        setupVisible = true;
        setContentView(wrapWithSystemBarInsets(scroll));
    }

    /**
     * 深色界面下的输入框：默认的 Material 下划线在 #080a0f 上几乎看不见，
     * 换成与网页端一致的深色圆角字段。
     */
    private void styleSetupField(EditText f) {
        GradientDrawable bg = new GradientDrawable();
        bg.setCornerRadius(dp(8));
        bg.setColor(Color.parseColor("#131824"));
        bg.setStroke(dp(1), Color.parseColor("#2a3444"));
        f.setBackground(bg);
        f.setPadding(dp(12), dp(12), dp(12), dp(12));
        f.setTextSize(TypedValue.COMPLEX_UNIT_SP, 15);
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.topMargin = dp(10);
        f.setLayoutParams(lp);
    }

    /**
     * 深色界面下的按钮：系统默认按钮是浅灰底白字，与背景几乎同色。
     * primary 用强调色，secondary 用与卡片一致的深色描边。
     */
    private void styleSetupButton(Button b, boolean primary) {
        GradientDrawable bg = new GradientDrawable();
        bg.setCornerRadius(dp(8));
        bg.setColor(Color.parseColor(primary ? "#2AABEE" : "#1e2634"));
        if (!primary) {
            bg.setStroke(dp(1), Color.parseColor("#2a3444"));
        }
        b.setBackground(bg);
        b.setTextColor(Color.parseColor(primary ? "#07121a" : "#e9edf5"));
        b.setAllCaps(false);
        b.setTextSize(TypedValue.COMPLEX_UNIT_SP, 15);
        b.setMinHeight(dp(44));
        b.setPadding(dp(12), dp(12), dp(12), dp(12));
    }

    // -------------------------------------------------------------- archive

    /**
     * Enters the archive, restarting the core if it was already running.
     *
     * <p>The Go side resolves api_id/api_hash when it constructs the client, so
     * a live instance keeps using the old pair no matter what SharedPreferences
     * says. Replacing credentials therefore means tearing the service down and
     * building a new one — a page reload alone would silently keep the old
     * application identity.
     */
    private void applyCredentials() {
        // 必须清掉 force_setup。onNewIntent 用 setIntent 把它写进了 intent，
        // 而 intent 在 Activity 重建时是保留的：不清的话保存完凭据以后，
        // 任何一次旋转或回收后恢复都会让 onCreate 再次命中 wantsSetup()，
        // 把一个正在正常归档的用户重新甩回「首次配置」页。
        Intent current = getIntent();
        if (current != null) {
            current.removeExtra(EXTRA_FORCE_SETUP);
        }
        if (!CoreHolder.isReady() && web == null) {
            showArchive();
            return;
        }
        ArchiveService.stop(this);
        CoreHolder.clear(CoreHolder.currentGeneration());
        // stopService is asynchronous and the old service's boot thread may
        // still be inside bridge.start — waiting on its destroy handshake
        // (bounded) beats a fixed guess. The wait happens off the main thread.
        new Thread(() -> {
            ArchiveService.awaitPreviousDestroy(2000);
            handler.post(this::showArchive);
        }, "tdl-restart-wait").start();
    }

    private void showArchive() {
        setupVisible = false;
        if (web != null) {
            // Reached when the user replaced credentials while the core was
            // already running; the old page is tied to the old instance.
            if (web.getParent() instanceof ViewGroup) {
                ((ViewGroup) web.getParent()).removeView(web);
            }
            web.destroy();
            web = null;
        }
        loaded = false;

        FrameLayout frame = new FrameLayout(this);
        frame.setBackgroundColor(Color.parseColor("#080a0f"));

        web = new WebView(this);
        WebSettings s = web.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true);
        // 页面是给手机写的应用界面，不是文档：viewport 已声明
        // width=device-width，再允许双指缩放只会让用户不小心把界面放大后
        // 找不到还原入口（缩回去要靠双击，多数人不知道）→ "UI 变奇怪了"。
        s.setSupportZoom(false);
        s.setBuiltInZoomControls(false);
        s.setDisplayZoomControls(false);
        // 有些 ROM 的 WebView 默认按系统字体缩放整体放大页面，会把 CSS 里
        // 精心算过的断点布局顶破；但直接把缩放钉死 100% 又会无视用户在系统
        // 设置里调大的字体（无障碍场景里等于关掉了放大镜）。取折中：跟随
        // fontScale，但限制在 85%–130%，既保住布局又让视力不好的用户看得清。
        applyTextZoom(s);
        // 页面里的「更改 API 凭据」入口需要回到原生配置页：WebView 里没有
        // 别的办法重新输入 api_id/api_hash。只暴露这一个方法，不开放文件、
        // 网络等能力。
        web.addJavascriptInterface(new ShellBridge(), "TdlAndroid");
        // Media in the archive is played from the loopback server; requiring a
        // gesture for every playback would fight the user's own tap.
        s.setMediaPlaybackRequiresUserGesture(false);
        // The page is local and trusted, but nothing in it needs file:// access,
        // so keep both channels closed.
        s.setAllowFileAccess(false);
        s.setAllowContentAccess(false);
        s.setCacheMode(WebSettings.LOAD_DEFAULT);
        web.setBackgroundColor(Color.parseColor("#080a0f"));
        // 默认的 WebViewClient 不拦导航。后果在手机上很具体：
        //   · target="_blank" 的链接（"打开文件"、链接卡片、按钮卡片）会在这个
        //     WebView 里原地导航 —— 外部站点把 SPA 顶掉，/media/<sha> 则把原始
        //     字节铺满屏幕；而页面里的 __tdlGoBack 拿不到历史，只能 finish()
        //     退出 App。
        //   · location.href="/api/export?..." 是 Content-Disposition: attachment，
        //     WebView 直接忽略，点了没任何反应。
        web.setWebViewClient(new WebViewClient() {
            @Override
            public boolean shouldOverrideUrlLoading(WebView v, WebResourceRequest req) {
                return handleNavigation(req.getUrl());
            }

            @Override
            public void onPageFinished(WebView v, String url) {
                // 没开 setSupportMultipleWindows 时 window.open() 直接返回 null，
                // 灯箱里"点大图看原图"就是静默失灵。转成同页导航，交给上面的
                // shouldOverrideUrlLoading 去取文件。
                v.evaluateJavascript(
                        "(function(){if(window.__tdlWinOpen)return;window.__tdlWinOpen=1;"
                                + "window.open=function(u){if(u)location.href=u;return null;};})()",
                        null);
            }
        });
        // confirm()/prompt()/alert() 默认由 WebView 自己画一个浏览器样式的
        // 弹窗，跟 App 视觉完全割裂（部分 ROM 上标题还写着「来自网页」）。
        // 接到原生 Material 对话框上：前端 13 处调用一行都不用改，但弹出来
        // 的是这个 App 自己的对话框。
        web.setWebChromeClient(new WebChromeClient() {
            @Override
            public boolean onJsConfirm(WebView v, String url, String message, JsResult result) {
                return showJsDialog(message, null, result, false);
            }

            @Override
            public boolean onJsAlert(WebView v, String url, String message, JsResult result) {
                return showJsDialog(message, null, result, false);
            }

            @Override
            public boolean onJsPrompt(WebView v, String url, String message,
                                      String defaultValue, JsPromptResult result) {
                return showJsDialog(message, defaultValue, result, true);
            }
        });
        web.setLayoutParams(new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
        frame.addView(web);

        // 启动屏。核心不是瞬间就绪的（SQLite 迁移 + 首次 MTProto 拨号在慢机上
        // 要好几秒），把 WebView 直接摆上去就是几秒的空白。用原生的一屏顶住，
        // 加载完成再让位；这也给了「正在启动（Ns）」一个像样的落点。
        LinearLayout splash = new LinearLayout(this);
        splash.setOrientation(LinearLayout.VERTICAL);
        splash.setGravity(Gravity.CENTER);
        splash.setLayoutParams(new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));

        TextView brand = new TextView(this);
        brand.setText(getString(R.string.app_name));
        brand.setTextColor(Color.parseColor("#e8eef7"));
        brand.setTextSize(TypedValue.COMPLEX_UNIT_SP, 20);
        brand.setGravity(Gravity.CENTER);
        splash.addView(brand);

        status = new TextView(this);
        status.setText(getString(R.string.starting));
        status.setTextColor(Color.parseColor("#8fa3bf"));
        status.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        status.setGravity(Gravity.CENTER);
        status.setPadding(0, dp(10), 0, 0);
        splash.addView(status);

        frame.addView(splash);

        archiveRoot = wrapWithSystemBarInsets(frame);
        setContentView(archiveRoot);
        // 从凭据页切回来时键盘可能刚收起，等这一帧的动画结束再按当前
        // insets 重算一次，页面不会被上一次的输入法高度压矮。
        frame.postDelayed(this::refreshInsets, 350);

        ArchiveService.start(this);
        waitStartedAt = System.currentTimeMillis();
        pollForCore();
    }

    private void pollForCore() {
        handler.postDelayed(() -> {
            if (isFinishing() || isDestroyed()) {
                return;
            }
            if (CoreHolder.isReady()) {
                loadArchive(CoreHolder.getAddr());
                return;
            }
            String err = CoreHolder.getError();
            if (err != null && !err.isEmpty()) {
                showCoreError(err);
                return;
            }
            long waited = System.currentTimeMillis() - waitStartedAt;
            if (waited > WAIT_TIMEOUT_MS) {
                if (CoreHolder.isStarting()) {
                    // 核心还在启动（SQLite 迁移/首次拨号在慢机上可能超过 60s）：
                    // 服务侧状态是可信的，UI 不能在这里永久放弃——那之后只剩
                    // 「启动超时」假死屏，重开应用是唯一出路。显示已等待秒数。
                    if (status != null) {
                        status.setText(getString(R.string.starting) + "（"
                                + (waited / 1000) + "s）");
                    }
                    pollForCore();
                    return;
                }
                showCoreError("启动超时：核心在 60 秒内没有就绪。");
                return;
            }
            if (status != null && waited > 3000) {
                status.setText(getString(R.string.starting) + "（" + (waited / 1000) + "s）");
            }
            pollForCore();
        }, POLL_INTERVAL_MS);
    }

    private void loadArchive(String addr) {
        if (loaded || web == null) {
            return;
        }
        loaded = true;
        if (status != null) {
            // status 现在挂在启动屏容器里，只藏 TextView 会把品牌标题留在
            // WebView 上面。整块一起收掉。
            if (status.getParent() instanceof View) {
                ((View) status.getParent()).setVisibility(View.GONE);
            }
            status.setVisibility(View.GONE);
        }
        // 回环服务带 per-boot token 鉴权：把 token 写进 WebView 的 cookie
        // （服务端同时接受 Bearer 头与 tdl_token cookie），主文档、静态子
        // 资源与页面里的 fetch()/XHR 就全部自动携带——shouldInterceptRequest
        // 拿不到 POST body，逐请求转发 Authorization 头是走不通的。
        String token = CoreHolder.getToken();
        if (token != null && !token.isEmpty()) {
            CookieManager cm = CookieManager.getInstance();
            String base = CORE_URL_PREFIX + addr;
            // 顺序不能反。acceptCookie 为 false 时 setCookie 会被静默丢弃：
            // 不抛异常、不写日志，token 就是进不了请求头，服务端一律 401，
            // 页面表现为没有任何报错的白屏。
            cm.setAcceptCookie(true);
            cm.setCookie(base, "tdl_token=" + token + "; Path=/; SameSite=Strict");
            cm.flush();
        }
        web.loadUrl(CORE_URL_PREFIX + addr + "/");
    }

    /**
     * 让网页文字跟着系统字体大小走，同时夹在一个布局能承受的区间里。
     *
     * <p>AndroidManifest 声明了 configChanges=fontScale，所以用户拖动字体
     * 滑块时 Activity 不会重建，必须在 onConfigurationChanged 里重新应用；
     * 否则 WebView 会一直停留在启动时的字号。
     */
    private void applyTextZoom(WebSettings s) {
        int zoom = (int) Math.round(getResources().getConfiguration().fontScale * 100f);
        if (zoom < 85) {
            zoom = 85;
        } else if (zoom > 130) {
            zoom = 130;
        }
        s.setTextZoom(zoom);
    }

    @Override
    public void onConfigurationChanged(Configuration newConfig) {
        super.onConfigurationChanged(newConfig);
        if (web != null) {
            try {
                applyTextZoom(web.getSettings());
            } catch (Throwable error) {
                logSuppressed("applyTextZoom", error);
            }
        }
    }

    /** 暴露给页面的最小桥：只够回到原生配置页，不碰文件与网络。 */
    private class ShellBridge {
        @JavascriptInterface
        public void openSetup() {
            handler.post(() -> showSetup(null));
        }
    }

    /**
     * Renders a JS dialog with the platform's own alert.
     *
     * <p>Returns true so the WebView does not also draw its browser-styled
     * fallback. Everything is guarded: showing a dialog from a finishing
     * activity throws, and a JsResult left unanswered blocks the page's
     * JavaScript thread forever — which is worse than the cosmetic problem
     * this exists to fix.
     */
    private boolean showJsDialog(String message, String defaultValue,
                                 JsResult result, boolean withInput) {
        try {
            AlertDialog.Builder b = new AlertDialog.Builder(this)
                    .setMessage(message)
                    .setOnCancelListener(dialog -> result.cancel());
            if (withInput) {
                final EditText input = new EditText(this);
                input.setText(defaultValue == null ? "" : defaultValue);
                b.setView(input)
                        .setPositiveButton(android.R.string.ok, (dialog, which) ->
                                ((JsPromptResult) result).confirm(input.getText().toString()))
                        .setNegativeButton(android.R.string.cancel,
                                (dialog, which) -> result.cancel());
            } else {
                b.setPositiveButton(android.R.string.ok,
                                (dialog, which) -> result.confirm())
                        .setNegativeButton(android.R.string.cancel,
                                (dialog, which) -> result.cancel());
            }
            b.show();
        } catch (Throwable error) {
            logSuppressed("showJsDialog", error);
            // Never leave the page's JS thread waiting on a dialog that
            // never appeared.
            result.cancel();
        }
        return true;
    }

    private void showCoreError(String message) {
        if (web != null) {
            web.setVisibility(View.GONE);
        }
        if (status != null) {
            status.setText(getString(R.string.start_failed) + "\n\n" + message
                    + "\n\n下拉通知栏可以看到服务状态；重新打开应用会重试。");
            status.setTextColor(Color.parseColor("#ff6b6b"));
        }
    }

    // -------------------------------------------------------------- android

    @Override
    protected void onPause() {
        // WebView 不知道自己被切到后台：lightbox 里的 <video autoplay> 会继续
        // 解码出声，配上常驻 wakelock 就是「锁屏后鬼故事音频」。pauseTimers
        // 停掉的是整个 WebView 的布局/解析时钟，回来时必须 resume。
        if (web != null) {
            web.onPause();
        }
        super.onPause();
    }

    @Override
    protected void onResume() {
        super.onResume();
        if (web != null) {
            web.onResume();
        }
        refreshInsets();
    }

    @Override
    public void onWindowFocusChanged(boolean hasFocus) {
        super.onWindowFocusChanged(hasFocus);
        // 键盘是在别的窗口里收起的时候，本窗口不一定会再收到一次 IME inset；
        // 拿回焦点时按当前状态重算，避免底部一直留着一块键盘高度的空白。
        if (hasFocus) {
            refreshInsets();
        }
    }

    /** 让 insets 容器按当前系统栏/输入法状态重新布局（无副作用）。 */
    private void refreshInsets() {
        View shell = insetShell;
        if (shell != null && shell.isAttachedToWindow()) {
            shell.requestApplyInsets();
        }
    }

    /**
     * Back is delegated to the page before the activity is allowed to finish.
     *
     * <p>web.canGoBack() reports the WebView's own navigation stack, which has
     * nothing to do with this SPA's router: the bottom bar navigates with
     * replaceState, so the stack usually holds a single entry and "back" either
     * does nothing at all or exits the app outright. Asking the page first lets
     * it close whatever it opened (sheet, lightbox, the long-press action row)
     * and only then hand the press back to the platform.
     */
    @Override
    public boolean onKeyDown(int keyCode, KeyEvent event) {
        if (keyCode == KeyEvent.KEYCODE_BACK) {
            // 凭据页把 WebView 换下去了，此时 web 仍然非空。若不单独处理，
            // 返回键会在后台页面求值 __tdlGoBack 得到 false，然后 finish()
            // 把整个 App 关掉——用户只是想从「更换凭据」回到归档。
            if (setupVisible) {
                if (archiveRoot != null) {
                    setupVisible = false;
                    // insetShell 已被凭据页那个 shell 覆盖（它已 detach），
                    // 指回去才能让 refreshInsets 重新生效。
                    insetShell = archiveRoot;
                    setContentView(archiveRoot);
                    refreshInsets();
                } else {
                    finish();
                }
                return true;
            }
            if (web != null) {
                web.evaluateJavascript(
                        "(typeof window.__tdlGoBack==='function')?window.__tdlGoBack():false",
                        handled -> {
                            // evaluateJavascript hands back a JSON literal.
                            if (!"true".equals(handled)) {
                                finish();
                            }
                        });
                return true;
            }
        }
        return super.onKeyDown(keyCode, event);
    }

    @Override
    protected void onDestroy() {
        // The service outlives the activity on purpose, so only release the
        // WebView here. Stopping the core would break background archiving.
        handler.removeCallbacksAndMessages(null);
        if (web != null) {
            // destroy() 契约要求先脱离视图树；带着 parent 销毁会泄漏渲染器
            if (web.getParent() instanceof ViewGroup) {
                ((ViewGroup) web.getParent()).removeView(web);
            }
            web.destroy();
            web = null;
        }
        super.onDestroy();
    }

    private void requestNotificationPermission() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) {
            return;
        }
        if (checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS)
                != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS},
                    REQ_NOTIFICATIONS);
        }
    }

    @Override
    public void onRequestPermissionsResult(int requestCode, String[] permissions,
                                           int[] grantResults) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults);
        if (requestCode != REQ_NOTIFICATIONS) {
            return;
        }
        boolean granted = grantResults.length > 0
                && grantResults[0] == PackageManager.PERMISSION_GRANTED;
        if (granted) {
            return;
        }
        // 权限被拒不影响归档本身（前台服务照样跑），但用户再也看不到常驻
        // 通知里的运行状态，也点不到里面的「更换凭据」。第一次拒绝给一条
        // 提示；如果系统已经不再弹窗（"不再询问"），直接指到设置页。
        if (!shouldShowRequestPermissionRationale(Manifest.permission.POST_NOTIFICATIONS)) {
            new AlertDialog.Builder(this)
                    .setTitle("通知权限已关闭")
                    .setMessage("归档仍会在后台继续，但状态栏看不到运行状态，"
                            + "也无法从通知里更换 API 凭据。可以在系统设置里重新打开。")
                    .setPositiveButton("去设置", (d, w) -> openNotificationSettings())
                    .setNegativeButton("暂时不用", null)
                    .show();
        } else {
            toast("已跳过通知权限：归档会继续，只是状态栏看不到运行状态");
        }
    }

    private void openNotificationSettings() {
        try {
            Intent intent = new Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS)
                    .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
                    .putExtra(Settings.EXTRA_APP_PACKAGE, getPackageName());
            startActivity(intent);
        } catch (Throwable error) {
            logSuppressed("openNotificationSettings", error);
            toast("打不开系统设置，请手动到应用信息里开启通知");
        }
    }

    // ---------------------------------------------------------------- crash

    /**
     * Writes Java stack traces to a file the app owns.
     *
     * <p>On this device logcat returns nothing at all — not for this app, not
     * for anything — and an uncaught Java exception leaves no tombstone either.
     * Without this hook a startup crash is completely silent: the process just
     * disappears and ExitInfo reports APP CRASH with trace=null. The file is
     * the only channel that survives.
     */
    /** Records a swallowed throwable, so a silent degradation is still traceable. */
    private void logSuppressed(String where, Throwable error) {
        try (PrintWriter out = new PrintWriter(
                new FileWriter(new File(getFilesDir(), "java-crash.log"), true))) {
            out.println("=== " + new Date() + "  SUPPRESSED in " + where);
            error.printStackTrace(out);
            out.println();
        } catch (Throwable ignored) {
        }
    }

    private void installJavaCrashLog() {
        // 只装一次。Activity 每次重建都会走 onCreate，而上面的 handler 会把
        // 上一个当成 previous 链式持有：装 N 次，一次崩溃就被写成 N 份，
        // 链越长日志越难读。进程内装一次就够了——handler 是 static 的。
        if (crashLogInstalled) {
            return;
        }
        crashLogInstalled = true;
        final File target = new File(getFilesDir(), "java-crash.log");
        final Thread.UncaughtExceptionHandler previous =
                Thread.getDefaultUncaughtExceptionHandler();
        Thread.setDefaultUncaughtExceptionHandler((thread, error) -> {
            try (PrintWriter out = new PrintWriter(new FileWriter(target, true))) {
                out.println("=== " + new Date() + "  thread=" + thread.getName());
                error.printStackTrace(out);
                out.println();
            } catch (Throwable ignored) {
                // Nowhere left to write; fall through to the default handler.
            }
            if (previous != null) {
                previous.uncaughtException(thread, error);
            }
        });
    }

    // --------------------------------------------------------------- insets

    /**
     * Keeps the status/navigation bar icons light.
     *
     * <p>Once the window paints edge-to-edge the bars have no background of
     * their own, so the system infers the icon colour from the app theme — and
     * on Android 15+ that inference lands on dark icons often enough to make
     * the clock and battery invisible against this app's near-black surface.
     * Clearing the LIGHT_* appearance bits pins them to light.
     */
    private void applyDarkSystemBarIcons() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.R) {
            return;
        }
        Window window = getWindow();
        if (window == null) {
            return;
        }
        WindowInsetsController controller = window.getInsetsController();
        if (controller == null) {
            return;
        }
        controller.setSystemBarsAppearance(0,
                WindowInsetsController.APPEARANCE_LIGHT_STATUS_BARS
                        | WindowInsetsController.APPEARANCE_LIGHT_NAVIGATION_BARS);
    }

    /**
     * Wraps a screen in a shell that reserves the system bars.
     *
     * <p>Android 15 (API 35) forces edge-to-edge on every app targeting it, and
     * Android 16 drops the opt-out property entirely — so the window always
     * paints underneath the status bar and the gesture bar. Plain ViewGroups
     * such as the FrameLayout and ScrollView used here never consume insets, so
     * the page's own top tab strip and bottom navigation end up exactly inside
     * those system gesture strips: painted, but every tap is swallowed by the
     * system before it reaches the view. Turning the insets into padding moves
     * the content back into the tappable area while keeping the edge-to-edge
     * paint.
     */
    private View wrapWithSystemBarInsets(View content) {
        try {
            return buildInsetShell(content);
        } catch (Throwable error) {
            // A wrong inset can make the UI awkward; a throw would make it
            // unusable. Degrade to the unwrapped view instead.
            logSuppressed("wrapWithSystemBarInsets", error);
            return content;
        }
    }

    private View buildInsetShell(View content) {
        FrameLayout shell = new FrameLayout(this);
        shell.setBackgroundColor(Color.parseColor("#080a0f"));
        shell.addView(content, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
        insetShell = shell;

        final int padLeft = shell.getPaddingLeft();
        final int padTop = shell.getPaddingTop();
        final int padRight = shell.getPaddingRight();
        final int padBottom = shell.getPaddingBottom();

        shell.setOnApplyWindowInsetsListener((v, insets) -> {
            int top;
            int bottom;
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                android.graphics.Insets bars = insets.getInsets(WindowInsets.Type.systemBars());
                android.graphics.Insets ime = insets.getInsets(WindowInsets.Type.ime());
                top = bars.top;
                // The keyboard has to be cleared as well, otherwise the api_id /
                // api_hash fields on the setup screen sit under it.
                bottom = Math.max(bars.bottom, ime.bottom);
            } else {
                top = insets.getSystemWindowInsetTop();
                bottom = insets.getSystemWindowInsetBottom();
            }
            v.setPadding(padLeft, padTop + top, padRight, padBottom + bottom);
            return insets;
        });
        shell.requestApplyInsets();
        return shell;
    }

    /**
     * 决定一次导航是留在 WebView 里，还是交给系统。
     *
     * @return true 表示已经处理掉了，WebView 不要自己导航
     */
    private boolean handleNavigation(Uri uri) {
        String scheme = uri.getScheme() == null ? "" : uri.getScheme().toLowerCase(Locale.ROOT);
        if (!"http".equals(scheme) && !"https".equals(scheme)) {
            // intent:/tel:/mailto: 之类丢给 WebView 只会得到 ERR_UNKNOWN_URL_SCHEME
            return true;
        }
        if (!isLoopback(uri)) {
            openExternally(uri);
            return true;
        }
        String path = uri.getPath() == null ? "" : uri.getPath();
        // 这两类是"取文件"，不是"换页面"：导出是 attachment，/media 是原始字节。
        // 交给 WebView 只会得到"点了没反应"或者"界面被顶掉"。
        if (path.startsWith("/api/export") || path.startsWith("/media/")) {
            fetchThenOpen(uri);
            return true;
        }
        return false; // SPA 内部导航
    }

    private static boolean isLoopback(Uri uri) {
        String host = uri.getHost();
        return "127.0.0.1".equals(host) || "localhost".equals(host) || "[::1]".equals(host);
    }

    private void openExternally(Uri uri) {
        try {
            startActivity(new Intent(Intent.ACTION_VIEW, uri));
        } catch (Throwable e) {
            logSuppressed("openExternally", e);
            toast("没有可以打开这个链接的应用");
        }
    }

    /**
     * 把回环服务上的一个文件取回本地，再交给系统打开。
     *
     * <p>为什么不是 DownloadManager：它跑在 com.android.providers.downloads
     * 进程里，用的是那个进程的网络安全策略。本应用只在
     * res/xml/network_security_config.xml 里对 127.0.0.1 放开了明文，对
     * DownloadManager 不生效 —— 它会直接拒掉 http://127.0.0.1。
     *
     * <p>取回的文件放 cacheDir/shared，由 {@link SharedFileProvider} 暴露成
     * content:// 再 ACTION_VIEW：Android 7 起 file:// 不能跨应用传。
     */
    private void fetchThenOpen(final Uri uri) {
        final String cookie = CookieManager.getInstance().getCookie(uri.toString());
        final File dir = SharedFileProvider.dir(this);
        new Thread(() -> {
            File saved = null;
            String failure = null;
            File part = null;
            HttpURLConnection conn = null;
            try {
                pruneSharedCache(dir);
                conn = (HttpURLConnection) new URL(uri.toString()).openConnection();
                conn.setConnectTimeout(10_000);
                conn.setReadTimeout(120_000);
                conn.setInstanceFollowRedirects(false);
                if (cookie != null && !cookie.isEmpty()) {
                    conn.setRequestProperty("Cookie", cookie);
                }
                int code = conn.getResponseCode();
                if (code != HttpURLConnection.HTTP_OK) {
                    throw new IOException("HTTP " + code);
                }
                long declared = conn.getContentLengthLong();
                // 声明大小就超限的不必开始读。没有 Content-Length 时靠下面的
                // 流式计数兜底，读到超限立刻中止。
                if (declared > MAX_DOWNLOAD_BYTES) {
                    throw new IOException("文件过大（" + formatBytes(declared)
                            + "），上限 " + formatBytes(MAX_DOWNLOAD_BYTES));
                }
                File out = new File(dir, pickFileName(conn, uri));
                ensureFreeSpace(dir, declared > 0 ? declared : 0);
                // 先写 .part 再改名：中途失败/被杀不会留下一个看起来完整的
                // 半截文件，用户点开时不会得到"文件已损坏"。
                part = new File(dir, out.getName() + ".part");
                long written = 0;
                try (InputStream in = conn.getInputStream();
                     OutputStream os = new FileOutputStream(part)) {
                    byte[] buf = new byte[64 * 1024];
                    int n;
                    while ((n = in.read(buf)) > 0) {
                        written += n;
                        if (written > MAX_DOWNLOAD_BYTES) {
                            throw new IOException("文件超过上限 "
                                    + formatBytes(MAX_DOWNLOAD_BYTES));
                        }
                        os.write(buf, 0, n);
                    }
                }
                if (declared >= 0 && written != declared) {
                    throw new IOException("下载不完整（" + written + "/" + declared + " 字节）");
                }
                //noinspection ResultOfMethodCallIgnored
                out.delete();
                if (!part.renameTo(out)) {
                    throw new IOException("无法保存文件 " + out.getName());
                }
                part = null;
                saved = out;
            } catch (Throwable e) {
                failure = e.toString();
                logSuppressed("fetchThenOpen", e);
            } finally {
                if (conn != null) {
                    conn.disconnect();
                }
                if (part != null) {
                    //noinspection ResultOfMethodCallIgnored
                    part.delete();
                }
            }
            final File done = saved;
            final String error = failure;
            handler.post(() -> {
                if (isFinishing() || isDestroyed()) {
                    return;
                }
                if (done == null) {
                    toast("下载失败：" + error);
                } else {
                    openSharedFile(done);
                }
            });
        }, "tdl-fetch").start();
    }

    /** Content-Disposition 里的文件名 → 磁盘上一个安全的文件名。 */
    private static String pickFileName(HttpURLConnection conn, Uri uri) {
        String name = null;
        String disposition = conn.getHeaderField("Content-Disposition");
        if (disposition != null) {
            Matcher m = CD_FILENAME.matcher(disposition);
            if (m.find()) {
                name = m.group(1);
            }
        }
        if (name == null || name.isEmpty()) {
            String last = uri.getLastPathSegment();
            name = (last == null || last.isEmpty()) ? "download" : last;
        }
        // 会直接变成文件名，也可能被塞进 content:// 的一段，先清掉分隔符。
        name = name.replace('\\', '_').replace('/', '_').replace('\0', '_').trim();
        if (name.isEmpty() || ".".equals(name) || "..".equals(name)) {
            name = "download";
        }
        // /media/<sha> 只给 Content-Type、不给 filename（web.go:1450）。不补扩展名
        // 的话文件名就是一串 sha，系统靠后缀认类型，ACTION_VIEW 会找不到任何
        // 能打开它的应用 —— 于是"打开文件"看起来还是坏的。
        String ext = extensionFor(conn.getContentType());
        if (ext != null && name.lastIndexOf('.') <= 0) {
            name = name + "." + ext;
        }
        if (name.length() > 120) {
            // 截断要保住扩展名：系统靠后缀挑打开方式，把 .mp4 截掉等于
            // 让 ACTION_VIEW 找不到应用。扩展名离谱长时才整体截断。
            int dot = name.lastIndexOf('.');
            if (dot > 0 && name.length() - dot <= 16) {
                String suffix = name.substring(dot);
                name = name.substring(0, 120 - suffix.length()) + suffix;
            } else {
                name = name.substring(0, 120);
            }
        }
        return name;
    }

    /**
     * 下载前清理 cacheDir/shared：先删过期文件，再按最旧优先压到总上限内。
     *
     * <p>这些文件只是为了交给外部应用打开；保留一整天足够，长期堆积却会
     * 吃掉用户的存储。
     */
    private static void pruneSharedCache(File dir) {
        File[] files = dir.listFiles();
        if (files == null) {
            return;
        }
        long now = System.currentTimeMillis();
        long total = 0;
        for (File f : files) {
            if (!f.isFile()) {
                continue;
            }
            if (now - f.lastModified() > SHARED_CACHE_TTL_MS) {
                // 过期文件可能是上次进程被杀留下的 .part，也可能只是放久
                // 了的导出；两者都不再需要。
                //noinspection ResultOfMethodCallIgnored
                f.delete();
                continue;
            }
            total += f.length();
        }
        if (total <= MAX_SHARED_CACHE_BYTES) {
            return;
        }
        Arrays.sort(files, (a, b) -> Long.compare(a.lastModified(), b.lastModified()));
        for (File f : files) {
            if (total <= MAX_SHARED_CACHE_BYTES) {
                break;
            }
            // 正在写的 .part 不参与容量淘汰：另一个下载线程还在往里写，
            // 删掉会让它写进一个已消失的文件。
            if (!f.isFile() || !f.exists() || f.getName().endsWith(".part")) {
                continue;
            }
            long size = f.length();
            if (f.delete()) {
                total -= size;
            }
        }
    }

    /** 下载前确认磁盘放得下，并留出 {@link #MIN_FREE_BYTES} 余量。 */
    private static void ensureFreeSpace(File dir, long needed) throws IOException {
        long required = needed + MIN_FREE_BYTES;
        StatFs stat = new StatFs(dir.getAbsolutePath());
        long available = stat.getAvailableBytes();
        if (available < required) {
            throw new IOException("存储空间不足：需要 " + formatBytes(required)
                    + "，可用 " + formatBytes(available));
        }
    }

    private static String formatBytes(long bytes) {
        if (bytes >= 1024L * 1024 * 1024) {
            return String.format(Locale.ROOT, "%.1f GB", bytes / (1024.0 * 1024 * 1024));
        }
        if (bytes >= 1024L * 1024) {
            return String.format(Locale.ROOT, "%.1f MB", bytes / (1024.0 * 1024));
        }
        if (bytes >= 1024) {
            return String.format(Locale.ROOT, "%.0f KB", bytes / 1024.0);
        }
        return bytes + " B";
    }

    /** 从 Content-Type 推一个扩展名（忽略 charset 等参数）。 */
    private static String extensionFor(String contentType) {
        if (contentType == null) {
            return null;
        }
        int semicolon = contentType.indexOf(';');
        String mime = (semicolon >= 0 ? contentType.substring(0, semicolon) : contentType)
                .trim().toLowerCase(Locale.ROOT);
        if (mime.isEmpty() || "application/octet-stream".equals(mime)) {
            return null;
        }
        String ext = MimeTypeMap.getSingleton().getExtensionFromMimeType(mime);
        return ext == null || ext.isEmpty() ? null : ext;
    }

    private void openSharedFile(File file) {
        try {
            Uri content = SharedFileProvider.uriFor(file);
            Intent view = new Intent(Intent.ACTION_VIEW);
            view.setDataAndType(content, SharedFileProvider.mimeOf(file));
            view.addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION);
            startActivity(view);
        } catch (Throwable e) {
            logSuppressed("openSharedFile", e);
            toast("没有能打开「" + file.getName() + "」的应用，文件已下载");
        }
    }

    private void toast(String text) {
        try {
            Toast.makeText(this, text, Toast.LENGTH_LONG).show();
        } catch (Throwable ignored) {
            // 通知失败不值得让整个流程失败
        }
    }

    private int dp(int value) {
        float density = getResources().getDisplayMetrics().density;
        return Math.round(value * density);
    }
}
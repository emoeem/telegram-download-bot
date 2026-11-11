package com.tdl.archive.app;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;

/**
 * Brings the archive core back after a reboot or an APK update.
 *
 * <p>Without this the core only ever runs while somebody has the app open: a
 * phone that restarts (or whose process is killed by a system update) silently
 * stops archiving until the next manual launch. For a tool whose whole point is
 * capturing messages continuously, that is the difference between an archive and
 * a partial one — and it fails silently, so nobody notices.
 *
 * <p>BOOT_COMPLETED is one of the cases Android exempts from the background
 * foreground-service start restriction, so starting the service from here is
 * allowed. Some ROMs still refuse, hence the catch: a missed autostart is bad,
 * a crash in a broadcast receiver is worse.
 */
public class BootReceiver extends BroadcastReceiver {

    @Override
    public void onReceive(Context context, Intent intent) {
        String action = intent == null ? null : intent.getAction();
        if (action == null) {
            return;
        }
        switch (action) {
            case Intent.ACTION_BOOT_COMPLETED:
            case Intent.ACTION_MY_PACKAGE_REPLACED:
            case "android.intent.action.QUICKBOOT_POWERON":
                break;
            default:
                return;
        }
        try {
            ArchiveService.start(context);
        } catch (Throwable error) {
            // Nothing useful to do: a broadcast receiver has nowhere to report
            // to, and letting it propagate only produces a crash dialog the
            // user cannot act on.
        }
    }
}

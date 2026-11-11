package com.tdl.archive.app;

import android.content.Context;
import android.os.Build;
import android.provider.Settings;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;

/**
 * Supplies the identification the licence binds to.
 *
 * <p>What is collected, and why each piece:
 *
 * <ul>
 *   <li>{@code ANDROID_ID} — per app-signing-key, per user, stable across app
 *       reinstalls on the same device. This is the component that makes "one
 *       key, one phone" survive a reinstall.
 *   <li>Build fingerprint, manufacturer, model, board — the coarse hardware
 *       identity. Together with ANDROID_ID they keep the code stable across a
 *       system update that does not move {@code ANDROID_ID}, while still
 *       differing between two physically distinct phones.
 * </ul>
 *
 * <p>Nothing here identifies the user and nothing is sent anywhere: the value is
 * hashed by the Go core into an 8-character device code that the activation
 * screen displays, and that code is what the customer reads to the vendor.
 *
 * <p>{@code ANDROID_ID} is read defensively. On a handful of ROMs it comes back
 * null or as the infamous all-zeros value; either would make every install on
 * that ROM share a device code, so a missing value falls back to the build
 * identity alone rather than producing a colliding constant.
 */
final class DeviceId {

    private DeviceId() {}

    /**
     * Builds the material string. Never returns null.
     *
     * <p>The separator is a control character that cannot appear in
     * {@code ANDROID_ID} or in a build property, so field boundaries are
     * unambiguous.
     */
    static String material(Context context) {
        StringBuilder sb = new StringBuilder(128);
        sb.append("android-id=").append(safeAndroidId(context));
        sb.append('\u0001').append("fp=").append(nullToEmpty(Build.FINGERPRINT));
        sb.append('\u0001').append("brand=").append(nullToEmpty(Build.BRAND));
        sb.append('\u0001').append("model=").append(nullToEmpty(Build.MODEL));
        sb.append('\u0001').append("device=").append(nullToEmpty(Build.DEVICE));
        sb.append('\u0001').append("board=").append(nullToEmpty(Build.BOARD));
        return sb.toString();
    }

    /**
     * Reads ANDROID_ID, rejecting the values that carry no information.
     *
     * <p>{@code 9774d56d682e549c} is the well-known constant returned by old
     * devices and by some custom ROMs, and {@code null}/all-zeros appear when
     * the setting is unreadable. Treating any of them as real would give every
     * affected install the same device code — the exact failure the licence
     * exists to prevent.
     */
    private static String safeAndroidId(Context context) {
        String id = null;
        try {
            id = Settings.Secure.getString(context.getContentResolver(), Settings.Secure.ANDROID_ID);
        } catch (Throwable ignored) {
            // A restricted profile can deny this read; fall through to build id.
        }
        if (id == null) {
            return "unavailable";
        }
        String trimmed = id.trim();
        if (trimmed.isEmpty()
                || "9774d56d682e549c".equalsIgnoreCase(trimmed)
                || "0000000000000000".equalsIgnoreCase(trimmed)
                || trimmed.matches("0+")) {
            return "unavailable";
        }
        return trimmed;
    }

    private static String nullToEmpty(String s) {
        return s == null ? "" : s;
    }

    /**
     * sha256 of the material, hex encoded.
     *
     * <p>Not used for the licence itself — the Go core does its own hashing.
     * Exposed so support can quote a short, non-reversible per-install
     * identifier without shipping the raw build properties.
     */
    static String fingerprint(Context context) {
        try {
            MessageDigest md = MessageDigest.getInstance("SHA-256");
            byte[] d = md.digest(material(context).getBytes(StandardCharsets.UTF_8));
            StringBuilder sb = new StringBuilder(d.length * 2);
            for (byte b : d) {
                sb.append(Character.forDigit((b >> 4) & 0xf, 16));
                sb.append(Character.forDigit(b & 0xf, 16));
            }
            return sb.toString();
        } catch (Throwable t) {
            return "";
        }
    }
}

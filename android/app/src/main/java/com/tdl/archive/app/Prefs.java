package com.tdl.archive.app;

import android.content.Context;
import android.content.SharedPreferences;
import android.security.keystore.KeyGenParameterSpec;
import android.security.keystore.KeyProperties;
import android.util.Base64;
import android.util.Log;

import java.nio.charset.StandardCharsets;
import java.security.KeyStore;

import javax.crypto.Cipher;
import javax.crypto.KeyGenerator;
import javax.crypto.SecretKey;
import javax.crypto.spec.GCMParameterSpec;

/**
 * Persisted Telegram application credentials.
 *
 * <p>api_id/api_hash are per-application values issued by Telegram, not per-user
 * secrets, but they are still worth protecting: a shared pair extracted from one
 * APK can be abused against every install of that build. User-supplied values
 * are therefore sealed with an AES-GCM key held in the Android Keystore, which
 * never leaves the TEE/StrongBox. Plaintext values written by older builds are
 * migrated on first read and removed afterwards.
 *
 * <p>The login session — which is far more sensitive — never comes here: it
 * lives in the Go KV store inside the app's private files directory, already
 * sealed by {@code archive.EncryptedStorage}.
 *
 * <p>A build can ship shared credentials via BuildConfig.TDL_APP_ID /
 * TDL_APP_HASH. Those are compiled into the APK and cannot be encrypted at rest;
 * the Keystore path only protects values the user enters.
 */
final class Prefs {

    private static final String TAG = "Prefs";
    private static final String NAME = "tdl_archive";

    // Legacy plaintext keys written by builds before Keystore encryption.
    private static final String KEY_APP_ID = "app_id";
    private static final String KEY_APP_HASH = "app_hash";
    // Sealed values: "enc:v1:" + base64(iv || ciphertext).
    private static final String KEY_APP_ID_ENC = "app_id_enc";
    private static final String KEY_APP_HASH_ENC = "app_hash_enc";
    private static final String KEY_PORT = "last_port";

    private static final String PREFIX = "enc:v1:";
    private static final String KEYSTORE = "AndroidKeyStore";
    private static final String ALIAS = "tdl_archive_creds_v1";
    private static final String TRANSFORMATION = "AES/GCM/NoPadding";
    private static final int GCM_TAG_BITS = 128;
    private static final int IV_BYTES = 12;

    private Prefs() {}

    private static SharedPreferences sp(Context c) {
        return c.getSharedPreferences(NAME, Context.MODE_PRIVATE);
    }

    /**
     * A value saved by the user always wins over the compiled-in default.
     *
     * <p>The order matters: the compiled-in pair is a shared credential that
     * every install of this build reuses, so an install that gets rate-limited
     * has no way to escape unless its own saved value takes precedence. When no
     * value is saved (a fresh install) the compiled-in pair is used, which is
     * what makes the first run require no setup screen.
     */
    static int getAppId(Context c) {
        int sealed = decryptInt(sp(c).getString(KEY_APP_ID_ENC, ""));
        if (sealed > 0) {
            return sealed;
        }
        int legacy = sp(c).getInt(KEY_APP_ID, 0);
        if (legacy > 0) {
            // Migrate the whole pair at once so neither half is left in
            // plaintext after this read.
            String hash = sp(c).getString(KEY_APP_HASH, "");
            if (hash != null && !hash.isEmpty()) {
                setCredentials(c, legacy, hash);
            }
            return legacy;
        }
        return BuildConfig.TDL_APP_ID;
    }

    static String getAppHash(Context c) {
        String sealed = decrypt(sp(c).getString(KEY_APP_HASH_ENC, ""));
        if (sealed != null && !sealed.isEmpty()) {
            return sealed;
        }
        String legacy = sp(c).getString(KEY_APP_HASH, "");
        if (legacy != null && !legacy.isEmpty()) {
            int id = sp(c).getInt(KEY_APP_ID, 0);
            if (id > 0) {
                setCredentials(c, id, legacy);
            }
            return legacy;
        }
        String builtin = BuildConfig.TDL_APP_HASH;
        return builtin == null ? "" : builtin;
    }

    static void setCredentials(Context c, int appId, String appHash) {
        String hash = appHash == null ? "" : appHash.trim();
        String sealedId = encrypt(Integer.toString(appId));
        String sealedHash = encrypt(hash);
        SharedPreferences.Editor editor = sp(c).edit();
        if (sealedId != null && sealedHash != null) {
            editor.putString(KEY_APP_ID_ENC, sealedId)
                    .putString(KEY_APP_HASH_ENC, sealedHash)
                    .remove(KEY_APP_ID)
                    .remove(KEY_APP_HASH);
        } else {
            // Keystore unavailable (rare vendor ROMs / broken keystore reset):
            // keep the feature working rather than silently losing the values.
            // The next successful setCredentials will migrate them back.
            Log.w(TAG, "keystore unavailable; storing credentials unsealed");
            editor.putInt(KEY_APP_ID, appId)
                    .putString(KEY_APP_HASH, hash)
                    .remove(KEY_APP_ID_ENC)
                    .remove(KEY_APP_HASH_ENC);
        }
        editor.apply();
    }

    /**
     * Drops the user-saved pair so the compiled-in one takes over again.
     *
     * <p>Without this the "keep using the built-in pair" action would be a lie:
     * saved values win, so simply restarting the core would keep the old pair.
     */
    static void clearCredentials(Context c) {
        sp(c).edit()
                .remove(KEY_APP_ID)
                .remove(KEY_APP_HASH)
                .remove(KEY_APP_ID_ENC)
                .remove(KEY_APP_HASH_ENC)
                .apply();
    }

    static boolean hasCredentials(Context c) {
        return getAppId(c) > 0 && !getAppHash(c).isEmpty();
    }

    static void setLastPort(Context c, int port) {
        sp(c).edit().putInt(KEY_PORT, port).apply();
    }

    static int getLastPort(Context c) {
        return sp(c).getInt(KEY_PORT, 0);
    }

    private static int decryptInt(String blob) {
        String plain = decrypt(blob);
        if (plain == null || plain.isEmpty()) {
            return 0;
        }
        try {
            return Integer.parseInt(plain);
        } catch (NumberFormatException e) {
            return 0;
        }
    }

    private static String encrypt(String plain) {
        try {
            Cipher cipher = Cipher.getInstance(TRANSFORMATION);
            cipher.init(Cipher.ENCRYPT_MODE, key());
            byte[] iv = cipher.getIV();
            byte[] ct = cipher.doFinal(plain.getBytes(StandardCharsets.UTF_8));
            byte[] out = new byte[iv.length + ct.length];
            System.arraycopy(iv, 0, out, 0, iv.length);
            System.arraycopy(ct, 0, out, iv.length, ct.length);
            return PREFIX + Base64.encodeToString(out, Base64.NO_WRAP);
        } catch (Exception e) {
            Log.w(TAG, "credential encryption failed", e);
            return null;
        }
    }

    private static String decrypt(String blob) {
        if (blob == null || !blob.startsWith(PREFIX)) {
            return null;
        }
        try {
            byte[] raw = Base64.decode(blob.substring(PREFIX.length()), Base64.NO_WRAP);
            if (raw.length <= IV_BYTES) {
                return null;
            }
            byte[] ct = new byte[raw.length - IV_BYTES];
            System.arraycopy(raw, IV_BYTES, ct, 0, ct.length);
            Cipher cipher = Cipher.getInstance(TRANSFORMATION);
            cipher.init(Cipher.DECRYPT_MODE, key(),
                    new GCMParameterSpec(GCM_TAG_BITS, raw, 0, IV_BYTES));
            return new String(cipher.doFinal(ct), StandardCharsets.UTF_8);
        } catch (Exception e) {
            // A wiped/renamed keystore makes old ciphertext unreadable. Falling
            // back to the compiled-in pair is the only non-crashing option; the
            // user can re-enter credentials from the settings screen.
            Log.w(TAG, "credential decryption failed", e);
            return null;
        }
    }

    private static SecretKey key() throws Exception {
        KeyStore ks = KeyStore.getInstance(KEYSTORE);
        ks.load(null);
        if (ks.containsAlias(ALIAS)) {
            return (SecretKey) ks.getKey(ALIAS, null);
        }
        KeyGenerator kg = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, KEYSTORE);
        kg.init(new KeyGenParameterSpec.Builder(ALIAS,
                KeyProperties.PURPOSE_ENCRYPT | KeyProperties.PURPOSE_DECRYPT)
                .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                .setKeySize(256)
                .setRandomizedEncryptionRequired(true)
                .build());
        return kg.generateKey();
    }
}

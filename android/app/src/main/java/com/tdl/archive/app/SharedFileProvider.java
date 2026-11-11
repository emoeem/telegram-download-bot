package com.tdl.archive.app;

import android.content.ContentProvider;
import android.content.ContentValues;
import android.content.Context;
import android.database.Cursor;
import android.database.MatrixCursor;
import android.net.Uri;
import android.os.ParcelFileDescriptor;
import android.provider.OpenableColumns;
import android.webkit.MimeTypeMap;

import java.io.File;
import java.io.FileNotFoundException;
import java.util.Locale;

/**
 * 把缓存目录里的一个文件暴露成 content:// URI，好让别的应用能打开它。
 *
 * <p>为什么不直接用 androidx.core.content.FileProvider：这个工程刻意不引
 * androidx（见 android/app/build.gradle 的 dependencies 注释——多一个 artifact
 * 会把 Kotlin stdlib 和一串传递依赖一起拖进来）。而 Android 7 起 file:// 已经
 * 不能跨应用传递，必须有 content:// 这一层。需要的能力就这么点，自己写比引一个
 * 框架小。
 *
 * <p>安全边界：只读、只在 cacheDir/shared 下取、文件名只允许
 * {@code [A-Za-z0-9._-]}，并且校验规范化后的父目录确实还是根目录——否则
 * `../` 就能把整个应用私有目录读出去。
 */
public class SharedFileProvider extends ContentProvider {

    static final String AUTHORITY = "com.tdl.archive.app.sharedfiles";

    private static final String DIR = "shared";

    /** 交给外部应用的文件目录；{@link #onCreate} 与取文件前都会确保存在。 */
    static File dir(Context ctx) {
        File d = new File(ctx.getCacheDir(), DIR);
        //noinspection ResultOfMethodCallIgnored
        d.mkdirs();
        return d;
    }

    static Uri uriFor(File file) {
        return new Uri.Builder()
                .scheme("content")
                .authority(AUTHORITY)
                .appendPath(file.getName())
                .build();
    }

    /** 按扩展名猜 MIME；猜不到就交给系统按字节嗅探。 */
    static String mimeOf(File file) {
        String name = file.getName();
        int dot = name.lastIndexOf('.');
        if (dot > 0 && dot < name.length() - 1) {
            String mime = MimeTypeMap.getSingleton()
                    .getMimeTypeFromExtension(name.substring(dot + 1).toLowerCase(Locale.ROOT));
            if (mime != null) {
                return mime;
            }
        }
        return "application/octet-stream";
    }

    private File root() {
        return new File(getContext().getCacheDir(), DIR).getAbsoluteFile();
    }

    private File resolve(Uri uri) throws FileNotFoundException {
        String name = uri.getLastPathSegment();
        // 不能只放行 ASCII：归档里的原始文件名经常是中文。这里挡的是
        // 路径分隔符和目录跳转，真正的边界由下面的 parent 校验兜底。
        if (name == null || name.isEmpty()
                || name.equals(".") || name.equals("..")
                || name.indexOf('/') >= 0 || name.indexOf('\\') >= 0
                || name.indexOf('\0') >= 0) {
            throw new FileNotFoundException("illegal name: " + name);
        }
        File root = root();
        File f = new File(root, name).getAbsoluteFile();
        if (!root.equals(f.getParentFile())) {
            throw new FileNotFoundException("escapes root: " + name);
        }
        if (!f.isFile()) {
            throw new FileNotFoundException("no such file: " + name);
        }
        return f;
    }

    @Override
    public boolean onCreate() {
        dir(getContext());
        return true;
    }

    @Override
    public ParcelFileDescriptor openFile(Uri uri, String mode) throws FileNotFoundException {
        if (mode != null && mode.contains("w")) {
            throw new FileNotFoundException("read-only provider");
        }
        return ParcelFileDescriptor.open(resolve(uri), ParcelFileDescriptor.MODE_READ_ONLY);
    }

    @Override
    public String getType(Uri uri) {
        String name = uri.getLastPathSegment();
        if (name == null) {
            return "application/octet-stream";
        }
        return mimeOf(new File(name));
    }

    /** 让接收方在保存/分享时看到真实文件名，而不是 URI 里那一段。 */
    @Override
    public Cursor query(Uri uri, String[] projection, String selection,
                        String[] selectionArgs, String sortOrder) {
        File f;
        try {
            f = resolve(uri);
        } catch (FileNotFoundException e) {
            return null;
        }
        String[] cols = projection == null
                ? new String[]{OpenableColumns.DISPLAY_NAME, OpenableColumns.SIZE}
                : projection;
        MatrixCursor cursor = new MatrixCursor(cols, 1);
        Object[] row = new Object[cols.length];
        for (int i = 0; i < cols.length; i++) {
            if (OpenableColumns.DISPLAY_NAME.equals(cols[i])) {
                row[i] = f.getName();
            } else if (OpenableColumns.SIZE.equals(cols[i])) {
                row[i] = f.length();
            } else {
                row[i] = null;
            }
        }
        cursor.addRow(row);
        return cursor;
    }

    @Override
    public Uri insert(Uri uri, ContentValues values) {
        return null;
    }

    @Override
    public int delete(Uri uri, String selection, String[] selectionArgs) {
        return 0;
    }

    @Override
    public int update(Uri uri, ContentValues values, String selection, String[] selectionArgs) {
        return 0;
    }
}

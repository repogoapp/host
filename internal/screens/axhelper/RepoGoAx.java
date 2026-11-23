// What's on an Android device's screen, by label, for the RepoGo host: run
// as shell through app_process, it connects UiAutomation once, as
// uiautomator's own shell wrapper does, then answers each "read" line on
// stdin with one JSON line on stdout. It holds the device's one UiAutomation
// client, so it lets go after 30 seconds without a read, and when stdin ends.
//
// Built by build.sh into repogo-ax.dex, which the host embeds.

import android.accessibilityservice.AccessibilityServiceInfo;
import android.app.UiAutomation;
import android.graphics.Rect;
import android.os.HandlerThread;
import android.os.Looper;
import android.view.accessibility.AccessibilityNodeInfo;
import android.view.accessibility.AccessibilityWindowInfo;
import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.lang.reflect.Constructor;
import java.lang.reflect.Method;
import java.util.List;

public class RepoGoAx {
  static final long IDLE_MS = 30_000;
  static volatile long lastRead = System.currentTimeMillis();
  static UiAutomation ua;
  static HandlerThread thread;

  // The main looper runs, as UiAutomation posts to it; the work runs on its own thread.
  public static void main(String[] args) {
    Looper.prepareMainLooper();
    new Thread(() -> {
      int code = 0;
      try {
        serve();
      } catch (Throwable e) {
        say("{\"error\":" + quote(String.valueOf(e.getCause() != null ? e.getCause() : e)) + "}");
        code = 1;
      } finally {
        disconnect();
      }
      System.exit(code);
    }).start();
    Looper.loop();
  }

  static void serve() throws Exception {
    connect();
    say("{\"ready\":true}");
    Thread idle = new Thread(() -> {
      while (true) {
        try {
          Thread.sleep(1000);
        } catch (InterruptedException e) {
          return;
        }
        if (System.currentTimeMillis() - lastRead > IDLE_MS) {
          disconnect();
          System.exit(0);
        }
      }
    });
    idle.setDaemon(true);
    idle.start();
    BufferedReader in = new BufferedReader(new InputStreamReader(System.in));
    String line;
    while ((line = in.readLine()) != null) {
      if (!line.trim().equals("read")) continue;
      lastRead = System.currentTimeMillis();
      say(read());
    }
  }

  static void connect() throws Exception {
    thread = new HandlerThread("RepoGoAx");
    thread.start();
    Class<?> connection = Class.forName("android.app.IUiAutomationConnection");
    Object conn = Class.forName("android.app.UiAutomationConnection").getDeclaredConstructor().newInstance();
    for (Constructor<?> c : UiAutomation.class.getDeclaredConstructors()) {
      Class<?>[] p = c.getParameterTypes();
      if (p.length == 2 && p[0] == Looper.class && p[1] == connection) {
        c.setAccessible(true);
        ua = (UiAutomation) c.newInstance(thread.getLooper(), conn);
      }
    }
    if (ua == null) throw new IllegalStateException("this Android has no UiAutomation(Looper, connection)");
    try {
      Method connect = UiAutomation.class.getDeclaredMethod("connect", int.class);
      connect.setAccessible(true);
      connect.invoke(ua, 1); // FLAG_DONT_SUPPRESS_ACCESSIBILITY_SERVICES
    } catch (NoSuchMethodException e) {
      Method connect = UiAutomation.class.getDeclaredMethod("connect");
      connect.setAccessible(true);
      connect.invoke(ua);
    }
    AccessibilityServiceInfo info = ua.getServiceInfo();
    info.flags |= AccessibilityServiceInfo.FLAG_INCLUDE_NOT_IMPORTANT_VIEWS
        | AccessibilityServiceInfo.FLAG_REPORT_VIEW_IDS
        | AccessibilityServiceInfo.FLAG_RETRIEVE_INTERACTIVE_WINDOWS;
    ua.setServiceInfo(info);
  }

  static void disconnect() {
    try {
      if (ua != null) {
        Method disconnect = UiAutomation.class.getDeclaredMethod("disconnect");
        disconnect.setAccessible(true);
        disconnect.invoke(ua);
      }
    } catch (Throwable ignored) {
    }
    ua = null;
    if (thread != null) thread.quitSafely();
  }

  // read answers the screen now: the node cache cleared first, or before
  // Android 14 (no clearCache) a new connection, so it never shows the last screen.
  static String read() throws Exception {
    try {
      ua.clearCache();
    } catch (NoSuchMethodError e) {
      disconnect();
      connect();
    }
    StringBuilder out = new StringBuilder("{\"elements\":[");
    int[] count = {0};
    // The windows can lag a moment behind a fresh connection.
    for (int attempt = 0; attempt < 10; attempt++) {
      List<AccessibilityWindowInfo> windows = ua.getWindows();
      for (AccessibilityWindowInfo w : windows) {
        AccessibilityNodeInfo root = w.getRoot();
        if (root != null) walk(root, out, count, 0);
      }
      if (windows.isEmpty()) {
        AccessibilityNodeInfo root = ua.getRootInActiveWindow();
        if (root != null) walk(root, out, count, 0);
      }
      if (count[0] > 0) break;
      Thread.sleep(100);
    }
    return out.append("]}").toString();
  }

  static void walk(AccessibilityNodeInfo n, StringBuilder out, int[] count, int depth) {
    if (depth > 64 || count[0] > 2000) return;
    CharSequence text = n.getText(), desc = n.getContentDescription(), hint = null;
    if (android.os.Build.VERSION.SDK_INT >= 26) hint = n.getHintText();
    String label = desc != null && desc.length() > 0 ? desc.toString() : text != null ? text.toString() : "";
    String value = text != null && desc != null && desc.length() > 0 ? text.toString() : "";
    if (n.isEditable()) {
      label = hint != null && hint.length() > 0 ? hint.toString() : label;
      value = text != null ? text.toString() : "";
    }
    String id = n.getViewIdResourceName();
    if (!label.isEmpty() || id != null || n.isEditable()) {
      Rect r = new Rect();
      n.getBoundsInScreen(r);
      if (r.width() > 0 && r.height() > 0) {
        String cls = n.getClassName() == null ? "" : n.getClassName().toString();
        if (count[0]++ > 0) out.append(',');
        out.append("{\"role\":").append(quote(cls.substring(cls.lastIndexOf('.') + 1)))
            .append(",\"label\":").append(quote(label))
            .append(",\"value\":").append(quote(value))
            .append(",\"id\":").append(quote(id == null ? "" : id))
            .append(",\"x\":").append(r.left).append(",\"y\":").append(r.top)
            .append(",\"w\":").append(r.width()).append(",\"h\":").append(r.height())
            .append(",\"selected\":").append(n.isSelected() || n.isChecked())
            .append('}');
      }
    }
    for (int i = 0; i < n.getChildCount(); i++) {
      AccessibilityNodeInfo c = n.getChild(i);
      if (c != null) walk(c, out, count, depth + 1);
    }
  }

  static synchronized void say(String line) {
    System.out.println(line);
    System.out.flush();
  }

  static String quote(String s) {
    StringBuilder b = new StringBuilder("\"");
    for (char c : s.toCharArray()) {
      switch (c) {
        case '"': b.append("\\\""); break;
        case '\\': b.append("\\\\"); break;
        case '\n': b.append("\\n"); break;
        case '\r': b.append("\\r"); break;
        case '\t': b.append("\\t"); break;
        default:
          if (c < 0x20) b.append(String.format("\\u%04x", (int) c));
          else b.append(c);
      }
    }
    return b.append('"').toString();
  }
}

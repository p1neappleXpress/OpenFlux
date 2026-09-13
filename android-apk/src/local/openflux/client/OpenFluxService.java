package local.openflux.client;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.content.Context;
import android.content.Intent;
import android.content.pm.ServiceInfo;
import android.net.Uri;
import android.net.VpnService;
import android.os.Build;
import android.os.Handler;
import android.os.Looper;
import android.os.ParcelFileDescriptor;
import org.json.JSONObject;
import java.io.BufferedReader;
import java.io.File;
import java.io.IOException;
import java.io.InputStreamReader;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.Locale;

public final class OpenFluxService extends VpnService {
    private static final String CHANNEL = "openflux_connection";
    private static final String STOP = "local.openflux.client.STOP";
    private static final StringBuilder LOG = new StringBuilder();
    private static volatile String state = "VPN отключён";
    private static volatile String traffic = "";
    private static volatile OpenFluxService current;
    private final Object lifecycle = new Object();
    private final Handler main = new Handler(Looper.getMainLooper());
    private volatile boolean stopping, finished;
    private volatile String coreExit;
    private Process process;
    private ParcelFileDescriptor tun;
    private long nativeHandle;
    private Thread worker;
    private Config config;
    private String lastBridgeError = "";

    static boolean isRunning() {
        OpenFluxService s = current;
        return s != null && s.worker != null && !s.stopping && !s.finished;
    }
    static String status() { return state; }
    static String traffic() { return traffic; }
    static String logs() { synchronized (LOG) { return LOG.toString(); } }

    @Override public void onCreate() {
        super.onCreate();
        current = this;
        NotificationChannel channel = new NotificationChannel(CHANNEL, "Соединение OpenFlux",
            NotificationManager.IMPORTANCE_LOW);
        channel.setDescription("Состояние VPN и отключение");
        getSystemService(NotificationManager.class).createNotificationChannel(channel);
    }

    @Override public int onStartCommand(Intent intent, int flags, int startId) {
        if (intent == null || STOP.equals(intent.getAction())) {
            stopSelf();
            return START_NOT_STICKY;
        }
        if (worker != null) return START_NOT_STICKY;
        synchronized (LOG) { LOG.setLength(0); }
        traffic = "";
        try {
            config = Config.from(intent);
            config.validate();
            Notification n = notification("Запуск VPN…");
            if (Build.VERSION.SDK_INT >= 34) {
                startForeground(1, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE);
            } else { startForeground(1, n); }
            state = "Запуск VPN…";
            worker = new Thread(this::runVPN, "openflux-vpn");
            worker.start();
        } catch (RuntimeException ex) {
            finished = true;
            state = "Ошибка запуска VPN";
            append(ex.toString());
            stopSelf();
        }
        return START_NOT_STICKY;
    }

    private PendingIntent openIntent() {
        return PendingIntent.getActivity(this, 0, new Intent(this, MainActivity.class),
            PendingIntent.FLAG_UPDATE_CURRENT | PendingIntent.FLAG_IMMUTABLE);
    }

    private void runVPN() {
        String result = "VPN завершён";
        long handle = 0;
        Thread logReader = null, watcher = null;
        try {
            if (VpnService.prepare(this) != null) throw new IllegalStateException("Нужно разрешение Android на VPN.");
            File executable = new File(getApplicationInfo().nativeLibraryDir, "libopenflux.so");
            if (!executable.isFile() || !executable.canExecute()) {
                throw new IllegalStateException("Не найден ARM64-клиент в APK.");
            }
            // All apps use the VPN. Only this UID is exempt, so OpenFlux's own
            // transport sockets reach Yandex/MAX instead of looping into the TUN.
            Builder builder = new Builder().setSession("OpenFlux VPN")
                .setMtu(config.mtu).addAddress("10.77.0.1", 30).addRoute("0.0.0.0", 0)
                .addDnsServer("10.77.0.2").addDisallowedApplication(getPackageName())
                .setBlocking(false).setConfigureIntent(openIntent());
            // No IPv6 address/route/DNS or allowFamily(AF_INET6): Android blocks
            // unsupported IPv6. Do not call allowBypass().
            synchronized (lifecycle) {
                if (stopping) return;
                tun = builder.establish();
                if (tun == null) throw new IllegalStateException("Android не создал интерфейс VPN.");
                handle = TunBridge.create(tun.getFd(), config.port, config.mtu);
                if (handle == 0) throw new IllegalStateException(TunBridge.lastError());
                nativeHandle = handle;
            }
            append("TUN включён для всех приложений. IPv4/TCP и DNS идут через OpenFlux.");
            append("MTU Android, TUN-моста и ядра OpenFlux: " + config.mtu);
            append("UDP, кроме DNS, и IPv6 заблокированы. После отключения VPN обычная сеть восстановится.");
            // Detect a port collision before accepting another process as the core.
            try (ServerSocket probe = new ServerSocket()) {
                probe.setReuseAddress(true);
                probe.bind(new InetSocketAddress(InetAddress.getByName("127.0.0.1"), config.port));
            }
            final Process child;
            synchronized (lifecycle) {
                if (stopping) return;
                ProcessBuilder core = new ProcessBuilder(config.arguments(executable.getAbsolutePath()))
                    .directory(getFilesDir()).redirectErrorStream(true);
                core.environment().put("OPENFLUX_MTU", Integer.toString(config.mtu));
                child = core.start();
                process = child;
            }
            logReader = new Thread(() -> readCoreLog(child), "openflux-log");
            logReader.start();
            final long session = handle;
            watcher = new Thread(() -> {
                try {
                    int code = child.waitFor();
                    if (!stopping) {
                        coreExit = "Ядро OpenFlux завершилось: " + code;
                        append(coreExit);
                        TunBridge.stop(session);
                    }
                } catch (InterruptedException ex) { Thread.currentThread().interrupt(); }
            }, "openflux-watch");
            watcher.start();
            waitForSocks(child);
            if (stopping) return;
            main.post(() -> {
                if (current == this && !stopping && !finished) {
                    state = "VPN включён";
                    append("Откройте сайт для проверки выхода в Интернет. Создание VPN ещё не подтверждает доступность ноды.");
                    getSystemService(NotificationManager.class).notify(1, notification("VPN для всех приложений"));
                    main.post(updateStats);
                }
            });
            int code = TunBridge.run(handle);
            if (coreExit != null) result = coreExit;
            else if (code != 0) throw new IOException(TunBridge.lastError());
        } catch (Exception | LinkageError ex) {
            result = "Ошибка VPN";
            if (!stopping) append(config.redact(ex.toString()));
        } finally {
            closeResources();
            if (handle != 0) TunBridge.destroy(handle);
            joinBriefly(logReader);
            joinBriefly(watcher);
            final String finalState = result;
            main.post(() -> {
                if (current == this && !stopping) {
                    finished = true;
                    state = finalState;
                    stopSelf();
                }
            });
        }
    }

    private void waitForSocks(Process child) throws Exception {
        long deadline = android.os.SystemClock.elapsedRealtime() + 20000;
        while (!stopping && android.os.SystemClock.elapsedRealtime() < deadline) {
            if (!child.isAlive()) throw new IOException("Ядро не запустилось. Проверьте журнал.");
            try (Socket socket = new Socket()) {
                socket.connect(new InetSocketAddress("127.0.0.1", config.port), 300);
                socket.setSoTimeout(1000);
                socket.getOutputStream().write(new byte[]{5, 1, 0});
                if (socket.getInputStream().read() == 5 && socket.getInputStream().read() == 0) return;
            } catch (IOException ignored) { }
            Thread.sleep(100);
        }
        if (!stopping) throw new IOException("Локальное ядро OpenFlux не ответило за 20 секунд.");
    }

    private void readCoreLog(Process child) {
        try (BufferedReader reader = new BufferedReader(new InputStreamReader(
            child.getInputStream(), StandardCharsets.UTF_8))) {
            String line;
            while ((line = reader.readLine()) != null) {
                if (stopping) break;
                // Keep transport diagnostics; omit per-packet metadata and destinations.
                if (line.contains("<- ") || line.contains("[SOCKS5] CONNECT ") || line.contains("[STATS]")) continue;
                append(config.redact(line));
                if (line.contains("config not found") || line.contains("editor_config nil")) {
                    append("Яндекс не вернул конфигурацию редактора. Нужна ссылка на совместимый документ, используемая выходной нодой.");
                }
            }
        } catch (IOException ex) { if (!stopping) append("Журнал ядра: " + config.redact(ex.getMessage())); }
    }

    private final Runnable updateStats = new Runnable() {
        @Override public void run() {
            if (current != OpenFluxService.this || stopping || finished) return;
            long handle;
            synchronized (lifecycle) { handle = nativeHandle; }
            if (handle == 0) return;
            try {
                JSONObject s = new JSONObject(TunBridge.stats(handle));
                traffic = String.format(Locale.ROOT, "Отправлено в VPN: %.1f КБ · получено: %.1f КБ\nTCP: %d · DNS: %d · ошибок DNS: %d",
                    s.optLong("up")/1024.0, s.optLong("down")/1024.0,
                    s.optLong("tcp"), s.optLong("dns"), s.optLong("dns_failed"));
                String error = s.optString("error");
                if (!error.isEmpty() && !error.equals(lastBridgeError)) {
                    lastBridgeError = error;
                    append(config.redact(error));
                }
            } catch (Exception ex) { append("Не удалось прочитать счётчики VPN."); }
            main.postDelayed(this, 1500);
        }
    };

    private void closeResources() {
        Process child;
        ParcelFileDescriptor descriptor;
        long handle;
        synchronized (lifecycle) {
            child = process; process = null;
            descriptor = tun; tun = null;
            handle = nativeHandle; nativeHandle = 0;
        }
        if (handle != 0) TunBridge.stop(handle);
        if (child != null) child.destroyForcibly();
        if (descriptor != null) { try { descriptor.close(); } catch (IOException ignored) {} }
    }

    private static void joinBriefly(Thread thread) {
        if (thread == null) return;
        try { thread.join(1500); } catch (InterruptedException ex) { Thread.currentThread().interrupt(); }
    }

    private void append(String message) {
        if (current != this || message == null) return;
        if (message.length() > 4096) message = message.substring(0, 4096) + "…";
        synchronized (LOG) {
            LOG.append(message).append('\n');
            if (LOG.length() > 24000) {
                int cut = LOG.indexOf("\n", LOG.length() - 20000);
                LOG.delete(0, cut >= 0 ? cut + 1 : LOG.length() - 20000);
            }
        }
    }

    private Notification notification(String text) {
        PendingIntent stop = PendingIntent.getService(this, 1,
            new Intent(this, OpenFluxService.class).setAction(STOP),
            PendingIntent.FLAG_UPDATE_CURRENT | PendingIntent.FLAG_IMMUTABLE);
        return new Notification.Builder(this, CHANNEL)
            .setSmallIcon(R.drawable.ic_status).setContentTitle("OpenFlux VPN")
            .setContentText(text).setContentIntent(openIntent()).setOngoing(true).setOnlyAlertOnce(true)
            .setVisibility(Notification.VISIBILITY_PRIVATE)
            .addAction(new Notification.Action.Builder(null, "Отключить", stop).build()).build();
    }

    @Override public void onRevoke() {
        append("Android отозвал VPN: разрешение отменено или включён другой VPN.");
        stopping = true;
        closeResources();
        stopSelf();
    }

    @Override public void onDestroy() {
        stopping = true;
        main.removeCallbacks(updateStats);
        closeResources();
        if (current == this) {
            if (!finished) { state = "VPN отключён"; append("VPN остановлен. Обычная маршрутизация восстановлена."); }
            current = null;
        }
        stopForeground(STOP_FOREGROUND_REMOVE);
        super.onDestroy();
    }
    // Inherit VpnService.onBind so Android can revoke the VPN correctly.
    static final class Config {
        final String transport, url, token, uid;
        final int port, mtu;
        Config(String transport, String url, String token, String uid, int port, int mtu) {
            this.transport = transport; this.url = url; this.token = token;
            this.uid = uid; this.port = port; this.mtu = mtu;
        }
        void validate() {
            if (mtu < 576 || mtu > 1500) throw new IllegalArgumentException("MTU: от 576 до 1500 байт.");
            if (port < 1024 || port > 65535) throw new IllegalArgumentException("Порт: от 1024 до 65535.");
            if ("yandex".equals(transport)) {
                Uri parsed = Uri.parse(url);
                if (!"https".equalsIgnoreCase(parsed.getScheme()) || parsed.getHost() == null)
                    throw new IllegalArgumentException("Укажите HTTPS-ссылку на документ Яндекса.");
            } else if ("oneme".equals(transport)) {
                if (token.isEmpty()) throw new IllegalArgumentException("Укажите токен MAX.");
                if (Long.parseLong(uid) <= 0) throw new IllegalArgumentException("UID MAX должен быть положительным числом.");
            } else {
                throw new IllegalArgumentException("Неизвестный транспорт.");
            }
        }
        Intent intent(Context context) {
            return new Intent(context, OpenFluxService.class)
                .putExtra("transport", transport).putExtra("url", url).putExtra("token", token)
                .putExtra("uid", uid).putExtra("port", port).putExtra("mtu", mtu);
        }
        static Config from(Intent intent) {
            return new Config(value(intent, "transport"), value(intent, "url"), value(intent, "token"),
                value(intent, "uid"), intent.getIntExtra("port", 1080), intent.getIntExtra("mtu", 1200));
        }
        private static String value(Intent i, String key) {
            String value = i.getStringExtra(key);
            return value == null ? "" : value;
        }
        List<String> arguments(String executable) {
            List<String> args = new ArrayList<>();
            args.add(executable); args.add("--client"); args.add("--debug");
            args.add("--transport"); args.add(transport);
            args.add("--socks5"); args.add("127.0.0.1:" + port);
            if ("yandex".equals(transport)) {
                args.add("--url"); args.add(url);
            } else {
                args.add("--maxToken"); args.add(token);
                args.add("--maxUid"); args.add(uid);
            }
            return args;
        }
        String redact(String text) {
            if (!token.isEmpty()) text = text.replace(token, "[токен скрыт]");
            if (!url.isEmpty()) text = text.replace(url, "[ссылка скрыта]");
            return text;
        }
    }
}

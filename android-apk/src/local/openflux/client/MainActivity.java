package local.openflux.client;

import android.Manifest;
import android.app.Activity;
import android.content.Intent;
import android.content.SharedPreferences;
import android.content.pm.PackageManager;
import android.graphics.Color;
import android.graphics.Typeface;
import android.graphics.Insets;
import android.net.VpnService;
import android.os.Build;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.text.InputType;
import android.view.View;
import android.view.WindowInsets;
import android.widget.AdapterView;
import android.widget.ArrayAdapter;
import android.widget.Button;
import android.widget.EditText;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.Spinner;
import android.widget.TextView;
import android.widget.Toast;

public final class MainActivity extends Activity {
    private static final String DEFAULT_URL = "https://disk.yandex.ru/i/2wk-vxDW74CAsA";
    private final Handler ui = new Handler(Looper.getMainLooper());
    private Spinner transport;
    private EditText url, token, uid, port, mtu;
    private LinearLayout yandexFields, maxFields;
    private TextView status, traffic, logs;
    private Button start, stop;
    private Intent pendingStart;
    private String lastLog = "";
    private SharedPreferences prefs;

    private final Runnable refresh = new Runnable() {
        @Override public void run() {
            boolean running = OpenFluxService.isRunning();
            start.setEnabled(!running && pendingStart == null);
            stop.setEnabled(running);
            transport.setEnabled(!running);
            mtu.setEnabled(!running);
            url.setEnabled(!running);
            token.setEnabled(!running);
            uid.setEnabled(!running);
            port.setEnabled(!running);
            mtu.setEnabled(!running);
            status.setText(OpenFluxService.status());
            traffic.setText(OpenFluxService.traffic());
            String value = OpenFluxService.logs();
            if (!lastLog.equals(value)) {
                lastLog = value;
                logs.setText(value.isEmpty() ? "Здесь появится журнал клиента." : value);
            }
            ui.postDelayed(this, 600);
        }
    };

    @Override public void onCreate(Bundle savedState) {
        super.onCreate(savedState);
        prefs = getSharedPreferences("profile", MODE_PRIVATE);

        LinearLayout frame = new LinearLayout(this);
        frame.setOrientation(LinearLayout.VERTICAL);
        frame.setBackgroundColor(Color.rgb(246, 248, 252));
        frame.setOnApplyWindowInsetsListener((view, insets) -> {
            if (Build.VERSION.SDK_INT >= 30) {
                Insets bars = insets.getInsets(WindowInsets.Type.systemBars() | WindowInsets.Type.ime());
                view.setPadding(bars.left, bars.top, bars.right, bars.bottom);
            } else {
                view.setPadding(insets.getSystemWindowInsetLeft(), insets.getSystemWindowInsetTop(),
                    insets.getSystemWindowInsetRight(), insets.getSystemWindowInsetBottom());
            }
            return insets;
        });
        ScrollView scroll = new ScrollView(this);
        scroll.setFillViewport(true);
        LinearLayout body = new LinearLayout(this);
        body.setOrientation(LinearLayout.VERTICAL);
        body.setPadding(dp(22), dp(22), dp(22), dp(32));
        scroll.addView(body);
        frame.addView(scroll, new LinearLayout.LayoutParams(-1, -1));
        setContentView(frame);
        frame.requestApplyInsets();

        TextView title = text("OpenFlux VPN", 28);
        title.setTypeface(Typeface.DEFAULT, Typeface.BOLD);
        body.addView(title);
        TextView description = text("VPN для всего устройства", 16);
        description.setTextColor(Color.rgb(80, 96, 118));
        body.addView(description);
        body.addView(text("Подключение через вашу ноду OpenFlux. После разрешения Android трафик приложений направляется в VPN: IPv4/TCP и DNS. UDP, кроме DNS, и IPv6 блокируются — игры и звонки, которым нужен UDP, могут не работать.", 14));

        status = text(OpenFluxService.status(), 17);
        status.setTypeface(Typeface.DEFAULT, Typeface.BOLD);
        status.setTextColor(Color.rgb(37, 99, 235));
        status.setPadding(0, dp(18), 0, dp(14));
        body.addView(status);
        traffic = text(OpenFluxService.traffic(), 12);
        body.addView(traffic);

        body.addView(text("Транспорт", 14));
        transport = new Spinner(this);
        ArrayAdapter<String> options = new ArrayAdapter<>(this,
            android.R.layout.simple_spinner_item, new String[]{"Яндекс Документы", "MAX"});
        options.setDropDownViewResource(android.R.layout.simple_spinner_dropdown_item);
        transport.setAdapter(options);
        body.addView(transport, new LinearLayout.LayoutParams(-1, dp(52)));

        yandexFields = group(body);
        yandexFields.addView(text("Ссылка на документ", 14));
        url = input(yandexFields, "https://…", InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_URI);
        String savedURL = prefs.getString("url", "");
        url.setText(savedURL.isEmpty() ? DEFAULT_URL : savedURL);

        maxFields = group(body);
        maxFields.addView(text("MAX: токен клиента", 14));
        token = input(maxFields, "Токен", InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_PASSWORD);
        token.setSaveEnabled(false);
        token.setImportantForAutofill(View.IMPORTANT_FOR_AUTOFILL_NO);
        maxFields.addView(text("MAX: UID собеседника на выходной ноде", 14));
        uid = input(maxFields, "Числовой UID", InputType.TYPE_CLASS_NUMBER);
        uid.setText(prefs.getString("uid", ""));
        maxFields.addView(text("Токен не сохраняется в настройках.", 12));

        transport.setOnItemSelectedListener(new AdapterView.OnItemSelectedListener() {
            @Override public void onItemSelected(AdapterView<?> parent, View v, int pos, long id) {
                yandexFields.setVisibility(pos == 0 ? View.VISIBLE : View.GONE);
                maxFields.setVisibility(pos == 1 ? View.VISIBLE : View.GONE);
            }
            @Override public void onNothingSelected(AdapterView<?> parent) {}
        });
        transport.setSelection(prefs.getInt("transport", 0));

        body.addView(text("Локальный порт · обычно 1080", 14));
        port = input(body, "1080", InputType.TYPE_CLASS_NUMBER);
        port.setText(prefs.getString("port", "1080"));

        body.addView(text("MTU туннеля · по умолчанию 1200", 14));
        mtu = input(body, "1200", InputType.TYPE_CLASS_NUMBER);
        mtu.setText(prefs.getString("mtu", "1200"));
        body.addView(text("Размер IP-пакета: 576–1500 байт. При message too long на сервере уменьшите MTU и переподключитесь.", 12));

        LinearLayout buttons = new LinearLayout(this);
        start = new Button(this);
        start.setText("Подключить VPN");
        start.setAllCaps(false);
        stop = new Button(this);
        stop.setText("Отключить");
        stop.setAllCaps(false);
        buttons.addView(start, new LinearLayout.LayoutParams(0, dp(56), 1));
        buttons.addView(stop, new LinearLayout.LayoutParams(0, dp(56), 1));
        body.addView(buttons);
        start.setOnClickListener(view -> startClient());
        stop.setOnClickListener(view -> stopService(new Intent(this, OpenFluxService.class)));

        TextView journalTitle = text("Журнал", 18);
        journalTitle.setTypeface(Typeface.DEFAULT, Typeface.BOLD);
        journalTitle.setPadding(0, dp(24), 0, dp(10));
        body.addView(journalTitle);
        logs = text("Здесь появится журнал клиента.", 12);
        logs.setTypeface(Typeface.MONOSPACE);
        logs.setTextIsSelectable(true);
        logs.setPadding(dp(12), dp(12), dp(12), dp(12));
        logs.setBackgroundColor(Color.WHITE);
        body.addView(logs, new LinearLayout.LayoutParams(-1, -2));
    }

    private void startClient() {
        try {
            OpenFluxService.Config c = new OpenFluxService.Config(
                transport.getSelectedItemPosition() == 0 ? "yandex" : "oneme",
                url.getText().toString().trim(), token.getText().toString().trim(),
                uid.getText().toString().trim(), Integer.parseInt(port.getText().toString().trim()),
                Integer.parseInt(mtu.getText().toString().trim()));
            c.validate();
            saveFields();
            pendingStart = c.intent(this);
            if (Build.VERSION.SDK_INT >= 33 && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS)
                != PackageManager.PERMISSION_GRANTED) {
                requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, 101);
            } else {
                launchPending();
            }
        } catch (NumberFormatException ex) {
            Toast.makeText(this, "Проверьте числовые значения порта, MTU и UID.", Toast.LENGTH_LONG).show();
        } catch (IllegalArgumentException ex) {
            Toast.makeText(this, ex.getMessage(), Toast.LENGTH_LONG).show();
        }
    }

    private void launchPending() {
        Intent request = pendingStart;
        if (request == null) return;
        try {
            Intent permission = VpnService.prepare(this);
            if (permission != null) {
                startActivityForResult(permission, 102);
                return;
            }
            pendingStart = null;
            startForegroundService(request);
            start.setEnabled(false);
        } catch (RuntimeException ex) {
            pendingStart = null;
            Toast.makeText(this, "Не удалось запустить службу: " + ex.getMessage(), Toast.LENGTH_LONG).show();
        }
    }

    @Override protected void onActivityResult(int requestCode, int resultCode, Intent data) {
        super.onActivityResult(requestCode, resultCode, data);
        if (requestCode == 102) {
            if (resultCode == RESULT_OK) launchPending();
            else {
                pendingStart = null;
                Toast.makeText(this, "Android не разрешил VPN. Подключение отменено.", Toast.LENGTH_LONG).show();
            }
        }
    }

    @Override public void onRequestPermissionsResult(int requestCode, String[] permissions, int[] results) {
        super.onRequestPermissionsResult(requestCode, permissions, results);
        // Foreground services may run when notification permission is denied.
        if (requestCode == 101) launchPending();
    }

    private void saveFields() {
        prefs.edit().putInt("transport", transport.getSelectedItemPosition())
            .putString("url", url.getText().toString().trim())
            .putString("uid", uid.getText().toString().trim())
            .putString("port", port.getText().toString().trim())
            .putString("mtu", mtu.getText().toString().trim()).apply();
    }

    @Override protected void onResume() {
        super.onResume();
        ui.removeCallbacks(refresh);
        ui.post(refresh);
    }

    @Override protected void onPause() {
        saveFields();
        ui.removeCallbacks(refresh);
        super.onPause();
    }

    private TextView text(String value, int size) {
        TextView v = new TextView(this);
        v.setText(value);
        v.setTextSize(size);
        v.setTextColor(Color.rgb(22, 35, 55));
        v.setPadding(0, dp(5), 0, dp(5));
        return v;
    }

    private EditText input(LinearLayout parent, String hint, int type) {
        EditText edit = new EditText(this);
        edit.setSingleLine(true);
        edit.setInputType(type);
        edit.setHint(hint);
        edit.setTextSize(16);
        parent.addView(edit, new LinearLayout.LayoutParams(-1, dp(54)));
        return edit;
    }

    private LinearLayout group(LinearLayout parent) {
        LinearLayout g = new LinearLayout(this);
        g.setOrientation(LinearLayout.VERTICAL);
        parent.addView(g, new LinearLayout.LayoutParams(-1, -2));
        return g;
    }

    private int dp(int value) {
        return Math.round(value * getResources().getDisplayMetrics().density);
    }
}

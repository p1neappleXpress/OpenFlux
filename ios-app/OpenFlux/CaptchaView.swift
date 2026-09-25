import SwiftUI
import WebKit

/// Interactive-captcha solver.
///
/// Yandex serves two kinds of captcha. The PoW one (`showcaptchafast`) the Go
/// core solves by itself. The other — SmartCaptcha (`showcaptcha?cc=1`) — needs
/// a real browser and a human, so the core stops, reports it, and waits for
/// fresh cookies. This screen is that step: load the document in a WKWebView,
/// let the user click through, then hand the resulting cookies back.
///
/// Detecting "solved" is deliberately loose: Yandex redirects around several
/// hosts, so instead of pattern-matching the flow we treat "we are no longer on
/// a captcha page" as success and also offer a manual button. Handing over
/// cookies is idempotent on the Go side, so an early or repeated hand-over is
/// harmless.
struct CaptchaView: View {
    let url: URL
    /// Called with cookies in Cookie-header form ("a=1; b=2").
    let onCookies: (String) -> Void

    @Environment(\.dismiss) private var dismiss
    @State private var status = "Пройдите проверку — куки подхватятся автоматически"
    @State private var busy = false
    @State private var looksSolved = false
    @StateObject private var model = CaptchaWebModel()

    var body: some View {
        NavigationView {
            VStack(spacing: 0) {
                CaptchaWebView(url: url, model: model)

                VStack(spacing: 8) {
                    if let e = model.loadError {
                        VStack(spacing: 6) {
                            Label("Страница не загрузилась", systemImage: "wifi.exclamationmark")
                                .font(.subheadline.bold()).foregroundColor(.orange)
                            Text(e).font(.caption2).foregroundColor(.secondary)
                                .multilineTextAlignment(.center)
                            Text("Капча появляется как раз когда туннель не работает, а трафик идёт через него. Маршруты для страницы капчи добавляются по DNS-ответу — первая попытка может не успеть, обновите.")
                                .font(.caption2).foregroundColor(.secondary)
                                .multilineTextAlignment(.center)
                            Button {
                                model.reload()
                            } label: {
                                Label("Обновить", systemImage: "arrow.clockwise")
                            }
                            .buttonStyle(.bordered)
                        }
                    }
                    Text(status)
                        .font(.caption2).foregroundColor(.secondary)
                        .multilineTextAlignment(.center)
                        .frame(maxWidth: .infinity)

                    Button {
                        handOver()
                    } label: {
                        Label(busy ? "Передаю…" : "Готово — передать куки",
                              systemImage: "checkmark.shield")
                            .frame(maxWidth: .infinity)
                    }
                    .buttonStyle(.borderedProminent)
                    .disabled(busy)
                }
                .padding()
                .background(Color(.secondarySystemBackground))
            }
            .navigationTitle("Проверка Яндекса")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Закрыть") { dismiss() }
                }
            }
            .onChange(of: model.lastURL) { newURL in
                // Left the captcha page: very likely solved. Hand cookies over
                // once, automatically, so the user does not have to guess.
                guard let s = newURL?.absoluteString else { return }
                let onCaptcha = s.contains("showcaptcha") || s.contains("captcha")
                if !onCaptcha && !looksSolved {
                    looksSolved = true
                    status = "Проверка пройдена — передаю куки…"
                    handOver()
                }
            }
        }
    }

    private func handOver() {
        busy = true
        model.collectCookies(for: url) { header in
            busy = false
            guard !header.isEmpty else {
                status = "Куки не найдены — пройдите проверку и нажмите ещё раз"
                return
            }
            onCookies(header)
            dismiss()
        }
    }
}

/// Observable state shared with the UIKit web view.
final class CaptchaWebModel: ObservableObject {
    @Published var lastURL: URL?
    /// Текст ошибки загрузки. Без него любая неудача выглядела как пустой белый
    /// экран: WKWebView просто ничего не рисует и молчит.
    @Published var loadError: String?
    @Published var loading = true
    fileprivate weak var webView: WKWebView?

    func reload() {
        loadError = nil
        loading = true
        webView?.reload()
    }

    /// Collects cookies for the document's host from the web view's data store
    /// and formats them as a Cookie header.
    ///
    /// Reads from WKWebsiteDataStore rather than the response headers because the
    /// captcha sets its cookie via JavaScript on some flows, where it never
    /// appears in a navigation response.
    func collectCookies(for url: URL, completion: @escaping (String) -> Void) {
        guard let store = webView?.configuration.websiteDataStore.httpCookieStore else {
            completion("")
            return
        }
        let host = url.host ?? ""
        store.getAllCookies { cookies in
            // Keep cookies for the document host and its parent domain: the
            // captcha is issued on yandex.ru while the document may live on
            // disk./docs.yandex.ru.
            let wanted = cookies.filter { c in
                let d = c.domain.hasPrefix(".") ? String(c.domain.dropFirst()) : c.domain
                return host == d || host.hasSuffix("." + d) || d.hasSuffix("yandex.ru") || d.hasSuffix("yandex.com")
            }
            let header = wanted.map { "\($0.name)=\($0.value)" }.joined(separator: "; ")
            DispatchQueue.main.async { completion(header) }
        }
    }
}

private struct CaptchaWebView: UIViewRepresentable {
    let url: URL
    let model: CaptchaWebModel

    func makeUIView(context: Context) -> WKWebView {
        let cfg = WKWebViewConfiguration()
        // Default (persistent) store on purpose: a non-persistent one would keep
        // the solved cookies out of reach after the sheet closes.
        cfg.websiteDataStore = .default()
        let web = WKWebView(frame: .zero, configuration: cfg)
        web.navigationDelegate = context.coordinator
        model.webView = web
        web.load(URLRequest(url: url))
        return web
    }

    func updateUIView(_ uiView: WKWebView, context: Context) {}

    func makeCoordinator() -> Coordinator { Coordinator(model: model) }

    final class Coordinator: NSObject, WKNavigationDelegate {
        let model: CaptchaWebModel
        init(model: CaptchaWebModel) { self.model = model }

        func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
            let u = webView.url
            DispatchQueue.main.async {
                self.model.lastURL = u
                self.model.loading = false
                self.model.loadError = nil
            }
        }

        func webView(_ webView: WKWebView, didStartProvisionalNavigation navigation: WKNavigation!) {
            DispatchQueue.main.async { self.model.loading = true }
        }

        // Обе ветки обязательны: провальная НАВИГАЦИЯ (не достучались до хоста)
        // и провал уже начатой загрузки приходят разными колбэками.
        func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!,
                     withError error: Error) {
            report(error)
        }

        func webView(_ webView: WKWebView, didFail navigation: WKNavigation!, withError error: Error) {
            report(error)
        }

        private func report(_ error: Error) {
            let ns = error as NSError
            // -999 = навигацию отменили (обычное дело при редиректах), это не сбой.
            guard ns.code != NSURLErrorCancelled else { return }
            DispatchQueue.main.async {
                self.model.loading = false
                self.model.loadError = "\(ns.localizedDescription) (код \(ns.code))"
            }
        }
    }
}

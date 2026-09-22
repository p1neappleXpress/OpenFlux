import SwiftUI
import UIKit
import AVFoundation
import CoreImage.CIFilterBuiltins

// MARK: - QR generation

/// Render a string as a crisp QR image (nil if it can't be built).
func makeQRImage(_ string: String) -> UIImage? {
    let filter = CIFilter.qrCodeGenerator()
    filter.message = Data(string.utf8)
    filter.correctionLevel = "M"
    guard let out = filter.outputImage else { return nil }
    let scaled = out.transformed(by: CGAffineTransform(scaleX: 10, y: 10))
    let ctx = CIContext()
    guard let cg = ctx.createCGImage(scaled, from: scaled.extent) else { return nil }
    return UIImage(cgImage: cg)
}

// MARK: - QR scanning (camera)

/// Full-screen camera QR scanner. Fires onScan once with the decoded string.
struct QRScannerView: UIViewControllerRepresentable {
    let onScan: (String) -> Void

    func makeUIViewController(context: Context) -> ScannerVC {
        let vc = ScannerVC()
        vc.onScan = onScan
        return vc
    }
    func updateUIViewController(_ vc: ScannerVC, context: Context) {}
}

final class ScannerVC: UIViewController, AVCaptureMetadataOutputObjectsDelegate {
    var onScan: ((String) -> Void)?
    private let session = AVCaptureSession()
    private var preview: AVCaptureVideoPreviewLayer?
    private var handled = false

    override func viewDidLoad() {
        super.viewDidLoad()
        view.backgroundColor = .black
        AVCaptureDevice.requestAccess(for: .video) { granted in
            DispatchQueue.main.async {
                if granted { self.configure() } else { self.showDenied() }
            }
        }
    }

    private func configure() {
        guard let device = AVCaptureDevice.default(for: .video),
              let input = try? AVCaptureDeviceInput(device: device),
              session.canAddInput(input) else { showDenied(); return }
        session.addInput(input)

        let output = AVCaptureMetadataOutput()
        guard session.canAddOutput(output) else { showDenied(); return }
        session.addOutput(output)
        output.setMetadataObjectsDelegate(self, queue: .main)
        output.metadataObjectTypes = [.qr]

        let pv = AVCaptureVideoPreviewLayer(session: session)
        pv.videoGravity = .resizeAspectFill
        pv.frame = view.layer.bounds
        view.layer.addSublayer(pv)
        preview = pv

        let hint = UILabel()
        hint.text = "Наведите камеру на QR-код конфигурации"
        hint.textColor = .white
        hint.textAlignment = .center
        hint.numberOfLines = 0
        hint.translatesAutoresizingMaskIntoConstraints = false
        view.addSubview(hint)
        NSLayoutConstraint.activate([
            hint.leadingAnchor.constraint(equalTo: view.leadingAnchor, constant: 24),
            hint.trailingAnchor.constraint(equalTo: view.trailingAnchor, constant: -24),
            hint.bottomAnchor.constraint(equalTo: view.safeAreaLayoutGuide.bottomAnchor, constant: -40),
        ])

        DispatchQueue.global(qos: .userInitiated).async { self.session.startRunning() }
    }

    private func showDenied() {
        let label = UILabel()
        label.text = "Нет доступа к камере.\nРазрешите его в Настройках iOS."
        label.textColor = .white
        label.textAlignment = .center
        label.numberOfLines = 0
        label.translatesAutoresizingMaskIntoConstraints = false
        view.addSubview(label)
        NSLayoutConstraint.activate([
            label.centerYAnchor.constraint(equalTo: view.centerYAnchor),
            label.leadingAnchor.constraint(equalTo: view.leadingAnchor, constant: 24),
            label.trailingAnchor.constraint(equalTo: view.trailingAnchor, constant: -24),
        ])
    }

    override func viewDidLayoutSubviews() {
        super.viewDidLayoutSubviews()
        preview?.frame = view.layer.bounds
    }

    override func viewWillDisappear(_ animated: Bool) {
        super.viewWillDisappear(animated)
        if session.isRunning { session.stopRunning() }
    }

    func metadataOutput(_ output: AVCaptureMetadataOutput,
                        didOutput metadataObjects: [AVMetadataObject],
                        from connection: AVCaptureConnection) {
        guard !handled,
              let obj = metadataObjects.first as? AVMetadataMachineReadableCodeObject,
              let s = obj.stringValue else { return }
        handled = true
        session.stopRunning()
        onScan?(s)
    }
}

// MARK: - Share a profile as a QR

/// Sheet showing a profile's OFLUX1 config as a QR + copyable text.
struct ShareQRView: View {
    let profile: Profile
    @Environment(\.dismiss) private var dismiss
    @State private var copied = false

    var body: some View {
        NavigationView {
            VStack(spacing: 20) {
                if let s = makeOFLUXString(profile), let img = makeQRImage(s) {
                    Text(profile.name).font(.headline)
                    Image(uiImage: img)
                        .interpolation(.none)
                        .resizable()
                        .scaledToFit()
                        .frame(maxWidth: 260, maxHeight: 260)
                        .padding(12)
                        .background(Color.white)
                        .clipShape(RoundedRectangle(cornerRadius: 12))
                    Button {
                        UIPasteboard.general.string = s
                        copied = true
                    } label: {
                        Label(copied ? "Скопировано" : "Скопировать OFLUX1", systemImage: copied ? "checkmark" : "doc.on.doc")
                    }
                    .buttonStyle(.bordered)
                    Text(s)
                        .font(.system(.caption2, design: .monospaced))
                        .foregroundColor(.secondary)
                        .multilineTextAlignment(.center)
                        .textSelection(.enabled)
                        .padding(.horizontal)
                } else {
                    Text("Для этого профиля нельзя сформировать ссылку (нужен документный транспорт).")
                        .foregroundColor(.secondary)
                        .multilineTextAlignment(.center)
                        .padding()
                }
                Spacer()
            }
            .padding()
            .navigationTitle("QR конфигурации")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("Готово") { dismiss() }
                }
            }
        }
        .navigationViewStyle(.stack)
    }
}

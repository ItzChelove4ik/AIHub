import AVFoundation
import SwiftUI
import UIKit

/// Scans an openflux:// QR code with the camera.
struct QRScannerSheet: View {
    var onCode: (String) -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var denied = false

    var body: some View {
        NavigationStack {
            Group {
                if denied {
                    ContentUnavailableView {
                        Label("Нет доступа к камере", systemImage: "camera.fill")
                    } description: {
                        Text("Разрешите доступ в Настройках или вставьте ссылку вручную.")
                    } actions: {
                        Button("Открыть Настройки") {
                            if let url = URL(string: UIApplication.openSettingsURLString) { UIApplication.shared.open(url) }
                        }
                    }
                } else {
                    QRScannerView { code in
                        if code.lowercased().hasPrefix("openflux://") { onCode(code) }
                    }
                    .ignoresSafeArea()
                }
            }
            .navigationTitle("QR-код канала")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar { Button("Отмена") { dismiss() } }
        }
        .task {
            switch AVCaptureDevice.authorizationStatus(for: .video) {
            case .authorized: denied = false
            case .notDetermined: denied = !(await AVCaptureDevice.requestAccess(for: .video))
            default: denied = true
            }
        }
    }
}

struct QRScannerView: UIViewControllerRepresentable {
    var onCode: (String) -> Void

    func makeUIViewController(context: Context) -> ScannerController {
        let c = ScannerController()
        c.onCode = onCode
        return c
    }

    func updateUIViewController(_ controller: ScannerController, context: Context) {
        controller.onCode = onCode
    }

    final class ScannerController: UIViewController, AVCaptureMetadataOutputObjectsDelegate {
        var onCode: ((String) -> Void)?
        private let session = AVCaptureSession()
        private var preview: AVCaptureVideoPreviewLayer?
        private var done = false

        override func viewDidLoad() {
            super.viewDidLoad()
            view.backgroundColor = .black
            guard let device = AVCaptureDevice.default(for: .video),
                  let input = try? AVCaptureDeviceInput(device: device),
                  session.canAddInput(input) else { return }
            session.addInput(input)
            let output = AVCaptureMetadataOutput()
            guard session.canAddOutput(output) else { return }
            session.addOutput(output)
            output.setMetadataObjectsDelegate(self, queue: .main)
            output.metadataObjectTypes = [.qr]
            let layer = AVCaptureVideoPreviewLayer(session: session)
            layer.videoGravity = .resizeAspectFill
            view.layer.addSublayer(layer)
            preview = layer
        }

        override func viewDidLayoutSubviews() {
            super.viewDidLayoutSubviews()
            preview?.frame = view.bounds
        }

        override func viewWillAppear(_ animated: Bool) {
            super.viewWillAppear(animated)
            // startRunning blocks: off the main thread (AVCaptureSession is
            // thread-safe for this).
            nonisolated(unsafe) let session = self.session
            DispatchQueue.global(qos: .userInitiated).async { session.startRunning() }
        }

        override func viewWillDisappear(_ animated: Bool) {
            super.viewWillDisappear(animated)
            session.stopRunning()
        }

        nonisolated func metadataOutput(_ output: AVCaptureMetadataOutput, didOutput metadataObjects: [AVMetadataObject],
                                        from connection: AVCaptureConnection) {
            let codes = metadataObjects.compactMap { ($0 as? AVMetadataMachineReadableCodeObject)?.stringValue }
            MainActor.assumeIsolated {
                guard !done, let code = codes.first else { return }
                done = true
                UINotificationFeedbackGenerator().notificationOccurred(.success)
                onCode?(code)
            }
        }
    }
}

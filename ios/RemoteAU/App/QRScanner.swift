import AVFoundation
import Combine
import SwiftUI
import UIKit

// MARK: - Pairing link

/// Parsed `remoteau://pair` deep link / QR payload used for secure v2 pairing.
///
/// Handled format:
/// `remoteau://pair?v=2&h=<ip>&p=<port>&id=<hex32>&c=<code>`
/// - `h` host (non-empty, required)
/// - `p` port (numeric, 1...65535, required)
/// - `c` pairing code (4-8 digits, required)
/// - `id` optional 32-char hex peer id; invalid ids are ignored
/// Unknown parameters are ignored.
struct PairingLink: Equatable {
    var host: String
    var port: Int
    var code: String
    var id: String?

    init(host: String, port: Int, code: String, id: String? = nil) {
        self.host = host
        self.port = port
        self.code = code
        self.id = id
    }

    /// Parses a raw QR payload. Returns `nil` for anything malformed.
    static func parse(_ raw: String) -> PairingLink? {
        let trimmed = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty,
              let comps = URLComponents(string: trimmed),
              comps.scheme?.lowercased() == "remoteau" else {
            return nil
        }

        let targetsPair = comps.host?.lowercased() == "pair" || comps.path == "/pair"
        guard targetsPair else { return nil }

        var params: [String: String] = [:]
        for item in comps.queryItems ?? [] {
            if let value = item.value, !value.isEmpty {
                params[item.name.lowercased()] = value
            }
        }

        guard let host = params["h"], !host.isEmpty else { return nil }
        guard let portRaw = params["p"], let port = Int(portRaw),
              (1...65535).contains(port) else {
            return nil
        }
        guard let code = params["c"],
              (4...8).contains(code.count),
              code.allSatisfy({ $0.isNumber }) else {
            return nil
        }

        var id: String?
        if let rawID = params["id"], rawID.count == 32,
           rawID.allSatisfy({ $0.isHexDigit }) {
            id = rawID.lowercased()
        }

        return PairingLink(host: host, port: port, code: code, id: id)
    }
}

// MARK: - QR capture controller

/// Drives an `AVCaptureSession` that scans QR codes for secure v2 pairing.
/// Session mutation happens on a private serial queue; published state is
/// always delivered on the main queue.
final class QRScanController: NSObject, ObservableObject,
                              AVCaptureMetadataOutputObjectsDelegate {
    @Published var scanned: String?
    @Published var active = false
    @Published var error: String?

    let session = AVCaptureSession()

    private let sessionQueue = DispatchQueue(label: "dev.remoteau.qr.session")
    private var configured = false
    private var lastPayload: String?
    private var lastPayloadAt: Date?

    /// Starts capture, requesting camera permission the first time.
    func start() {
        switch AVCaptureDevice.authorizationStatus(for: .video) {
        case .authorized:
            startSession()
        case .notDetermined:
            AVCaptureDevice.requestAccess(for: .video) { [weak self] granted in
                guard let self else { return }
                if granted {
                    self.startSession()
                } else {
                    DispatchQueue.main.async {
                        self.error = "Camera access was denied. Enable it in Settings to scan QR codes."
                    }
                }
            }
        case .denied, .restricted:
            error = "Camera access is unavailable. Enable it in Settings to scan QR codes."
        @unknown default:
            error = "Camera access is unavailable."
        }
    }

    /// Stops capture. Safe to call repeatedly.
    func stop() {
        sessionQueue.async { [weak self] in
            guard let self else { return }
            if self.session.isRunning {
                self.session.stopRunning()
            }
            DispatchQueue.main.async { self.active = false }
        }
    }

    /// Preview layer wired to this controller's session.
    func makePreviewLayer() -> AVCaptureVideoPreviewLayer {
        let layer = AVCaptureVideoPreviewLayer(session: session)
        layer.videoGravity = .resizeAspectFill
        return layer
    }

    private func startSession() {
        sessionQueue.async { [weak self] in
            guard let self else { return }
            if !self.configured {
                self.configureSession()
            }
            guard self.configured else { return }
            if !self.session.isRunning {
                self.session.startRunning()
            }
            DispatchQueue.main.async { self.active = true }
        }
    }

    /// Runs only on `sessionQueue`.
    private func configureSession() {
        guard let device = AVCaptureDevice.default(.builtInWideAngleCamera,
                                                   for: .video, position: .back) else {
            fail("No usable back camera was found.")
            return
        }
        guard let input = try? AVCaptureDeviceInput(device: device) else {
            fail("Unable to open the camera.")
            return
        }

        session.beginConfiguration()
        session.sessionPreset = .high

        guard session.canAddInput(input) else {
            session.commitConfiguration()
            fail("Unable to use the camera input.")
            return
        }
        session.addInput(input)

        let output = AVCaptureMetadataOutput()
        guard session.canAddOutput(output) else {
            session.commitConfiguration()
            fail("Unable to read camera frames.")
            return
        }
        session.addOutput(output)
        output.setMetadataObjectsDelegate(self, queue: sessionQueue)
        output.metadataObjectTypes = [.qr]
        session.commitConfiguration()
        configured = true
    }

    private func fail(_ message: String) {
        DispatchQueue.main.async { [weak self] in
            self?.error = message
        }
    }

    // MARK: AVCaptureMetadataOutputObjectsDelegate

    func metadataOutput(_ output: AVCaptureMetadataOutput,
                        didOutput metadataObjects: [AVMetadataObject],
                        from connection: AVCaptureConnection) {
        guard let object = metadataObjects.first as? AVMetadataMachineReadableCodeObject,
              object.type == .qr,
              let payload = object.stringValue,
              !payload.isEmpty else {
            return
        }

        // Ignore the same payload seen again within two seconds.
        let now = Date()
        if let last = lastPayload, last == payload,
           let at = lastPayloadAt, now.timeIntervalSince(at) < 2.0 {
            return
        }
        lastPayload = payload
        lastPayloadAt = now

        if session.isRunning {
            session.stopRunning()
        }
        DispatchQueue.main.async { [weak self] in
            guard let self else { return }
            self.active = false
            self.scanned = payload
        }
    }
}

// MARK: - SwiftUI wrapper

/// Hosts a full-screen QR camera preview. Starts/stops the controller as the
/// view appears and disappears. `onScan` fires on the main queue.
struct QRScannerView: UIViewControllerRepresentable {
    var onScan: (String) -> Void

    func makeUIViewController(context: Context) -> QRScannerViewController {
        let vc = QRScannerViewController()
        vc.onScan = onScan
        return vc
    }

    func updateUIViewController(_ uiViewController: QRScannerViewController,
                                context: Context) {
        uiViewController.onScan = onScan
    }
}

/// UIViewController that owns the capture controller and preview layer.
final class QRScannerViewController: UIViewController {
    var onScan: ((String) -> Void)?

    private let controller = QRScanController()
    private var previewLayer: AVCaptureVideoPreviewLayer?
    private var cancellables = Set<AnyCancellable>()

    private let instructionLabel = UILabel()
    private let errorLabel = UILabel()

    override func viewDidLoad() {
        super.viewDidLoad()
        view.backgroundColor = .black

        let layer = controller.makePreviewLayer()
        layer.frame = view.bounds
        view.layer.addSublayer(layer)
        previewLayer = layer

        instructionLabel.text = "Point the camera at the pairing QR code"
        instructionLabel.textColor = .white
        instructionLabel.font = .preferredFont(forTextStyle: .subheadline)
        instructionLabel.textAlignment = .center
        instructionLabel.numberOfLines = 0
        instructionLabel.translatesAutoresizingMaskIntoConstraints = false

        errorLabel.textColor = .systemOrange
        errorLabel.font = .preferredFont(forTextStyle: .footnote)
        errorLabel.textAlignment = .center
        errorLabel.numberOfLines = 0
        errorLabel.isHidden = true
        errorLabel.translatesAutoresizingMaskIntoConstraints = false

        view.addSubview(instructionLabel)
        view.addSubview(errorLabel)

        NSLayoutConstraint.activate([
            instructionLabel.leadingAnchor.constraint(equalTo: view.layoutMarginsGuide.leadingAnchor),
            instructionLabel.trailingAnchor.constraint(equalTo: view.layoutMarginsGuide.trailingAnchor),
            instructionLabel.bottomAnchor.constraint(equalTo: view.safeAreaLayoutGuide.bottomAnchor,
                                                     constant: -24),
            errorLabel.leadingAnchor.constraint(equalTo: view.layoutMarginsGuide.leadingAnchor),
            errorLabel.trailingAnchor.constraint(equalTo: view.layoutMarginsGuide.trailingAnchor),
            errorLabel.centerYAnchor.constraint(equalTo: view.centerYAnchor),
        ])

        controller.$scanned
            .compactMap { $0 }
            .receive(on: DispatchQueue.main)
            .sink { [weak self] payload in
                self?.onScan?(payload)
            }
            .store(in: &cancellables)

        controller.$error
            .receive(on: DispatchQueue.main)
            .sink { [weak self] message in
                guard let self else { return }
                self.errorLabel.text = message
                self.errorLabel.isHidden = message == nil
                self.instructionLabel.isHidden = message != nil
            }
            .store(in: &cancellables)
    }

    override func viewDidLayoutSubviews() {
        super.viewDidLayoutSubviews()
        previewLayer?.frame = view.bounds
    }

    override func viewWillAppear(_ animated: Bool) {
        super.viewWillAppear(animated)
        controller.start()
    }

    override func viewWillDisappear(_ animated: Bool) {
        super.viewWillDisappear(animated)
        controller.stop()
    }
}

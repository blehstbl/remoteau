import SwiftUI

// MARK: - Pairing code prompt bridge

/// Bridges the Go engine's on-demand request for the pairing code with a UI
/// prompt.
///
/// The host only generates and displays its PIN *after* the receiver has sent
/// `PAIR_BEGIN` — and it generates a fresh PIN for every connection attempt.
/// A code typed before connecting is therefore always stale: tapping Connect
/// starts a new attempt which replaces whatever the PC was showing. This type
/// makes the app wait until the engine actually asks for the code (which is
/// exactly when the PC has put it on screen), then returns the user's answer.
@MainActor
final class PINPrompt: ObservableObject {
    /// Drives the entry sheet. Set when the engine asks and cleared on submit.
    @Published var isPresented = false

    /// A code obtained without a prompt (e.g. a scanned QR). Consumed by the
    /// next `request()` instead of showing the sheet.
    var preloaded: String?

    private var continuation: CheckedContinuation<String, Never>?

    /// Called (via the Go PIN provider) once the host has shown its code.
    func request() async -> String {
        if let preloaded, !preloaded.isEmpty {
            self.preloaded = nil
            return preloaded
        }
        return await withCheckedContinuation { (c: CheckedContinuation<String, Never>) in
            self.continuation = c
            self.isPresented = true
        }
    }

    /// Resumes a pending request with the user's code.
    func submit(_ code: String) {
        isPresented = false
        let c = continuation
        continuation = nil
        c?.resume(returning: code)
    }

    /// Cancels a pending request by resuming with an empty code, which the
    /// exchange rejects as a PIN mismatch (a terminal, non-retrying failure).
    func cancel() {
        submit("")
    }
}

// MARK: - Entry sheet

/// Code entry shown while pairing. Presented on demand from `PINPrompt` so the
/// user types the code that is currently on the PC's screen.
struct PINEntrySheet: View {
    @ObservedObject var prompt: PINPrompt
    @Environment(\.dismiss) private var dismiss
    @State private var code = ""
    @State private var showScanner = false
    @FocusState private var focused: Bool

    var body: some View {
        NavigationStack {
            VStack(alignment: .leading, spacing: Theme.spacingL) {
                Text("Type the code shown on the PC to finish pairing. The PC's code and QR are valid only for this connection attempt.")
                    .font(.subheadline)
                    .foregroundStyle(.secondary)

                TextField("Pairing code", text: $code)
                    .textFieldStyle(.roundedBorder)
                    .keyboardType(.numberPad)
                    .font(.title2.monospacedDigit())
                    .multilineTextAlignment(.center)
                    .focused($focused)
                    .accessibilityLabel("Pairing code")

                Button {
                    prompt.submit(code)
                    dismiss()
                } label: {
                    Text("Pair")
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(.borderedProminent)
                .disabled(code.count < 4)

                Button {
                    showScanner = true
                } label: {
                    Label("Scan QR Code", systemImage: "qrcode.viewfinder")
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(.bordered)
                .accessibilityLabel("Scan pairing QR code")

                Spacer()
            }
            .padding(Theme.spacingL)
            .navigationTitle("Enter pairing code")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") {
                        prompt.cancel()
                        dismiss()
                    }
                }
            }
            .sheet(isPresented: $showScanner) {
                NavigationStack {
                    QRScannerView { raw in
                        showScanner = false
                        if let link = PairingLink.parse(raw), !link.code.isEmpty {
                            code = link.code
                        }
                    }
                    .ignoresSafeArea()
                    .navigationTitle("Scan QR code")
                    .navigationBarTitleDisplayMode(.inline)
                    .toolbar {
                        ToolbarItem(placement: .cancellationAction) {
                            Button("Cancel") { showScanner = false }
                        }
                    }
                }
            }
            .onAppear { focused = true }
        }
    }
}

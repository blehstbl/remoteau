import SwiftUI

/// Design system: spacing, corner radii, semantic colors. Adapts to dark and
/// light mode via system semantic colors.
enum Theme {
    static let cornerL: CGFloat = 22
    static let cornerM: CGFloat = 16
    static let cornerS: CGFloat = 10

    static let spacingL: CGFloat = 20
    static let spacingM: CGFloat = 12
    static let spacingS: CGFloat = 8

    static func statusColor(for state: ReceiverModel.ConnectionState) -> Color {
        switch state {
        case .idle: return .secondary
        case .waitingForSender: return .orange
        case .streaming: return .green
        case .interrupted: return .yellow
        }
    }

    static func qualityLabel(for loss: Double, jitter: Double) -> String {
        switch (loss, jitter) {
        case (<1, <6): return "Excellent"
        case (<3, <15): return "Good"
        case (<6, <40): return "Fair"
        default: return "Poor"
        }
    }
}

/// Subtle pulsing dot used for live connection states.
struct PulseDot: View {
    var color: Color
    var active: Bool = true
    @State private var pulse = false

    var body: some View {
        Circle()
            .fill(color)
            .frame(width: 10, height: 10)
            .overlay(
                Circle()
                    .stroke(color.opacity(0.6), lineWidth: 2)
                    .scaleEffect(pulse ? 1.9 : 1.0)
                    .opacity(pulse ? 0 : 0.8)
                    .animation(active ? Animation.easeOut(duration: 1.4).repeatForever(autoreverses: false) : nil,
                               value: pulse)
            )
            .onAppear { if active { pulse = true } }
            .accessibilityHidden(true)
    }
}

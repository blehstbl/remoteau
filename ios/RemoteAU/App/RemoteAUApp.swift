import SwiftUI

@main
struct RemoteAUApp: App {
    @StateObject private var model = ReceiverModel()

    var body: some Scene {
        WindowGroup {
            RootView()
                .environmentObject(model)
                .preferredColorScheme(nil) // follow system (dark/light)
        }
    }
}

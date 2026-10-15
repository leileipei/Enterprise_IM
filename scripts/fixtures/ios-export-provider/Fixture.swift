import UIKit

// Disposable simulator-only document source. Contains synthetic files only;
// never bundled into the IM client or used for customer document storage.
@main final class ExportFixtureApp: UIResponder, UIApplicationDelegate {
  func application(_ application: UIApplication, didFinishLaunchingWithOptions options: [UIApplication.LaunchOptionsKey: Any]?) -> Bool {
    do {
      let documents = FileManager.default.urls(for: .documentDirectory, in: .userDomainMask)[0]
      let target = documents.appendingPathComponent("ExportTarget", isDirectory: true)
      try FileManager.default.createDirectory(at: target, withIntermediateDirectories: true)
      let sentinel = target.appendingPathComponent("existing.txt")
      if !FileManager.default.fileExists(atPath: sentinel.path) {
        try Data("enterprise-im-fixture-preserve\n".utf8).write(to: sentinel, options: .withoutOverwriting)
      }
    } catch { return false }
    return true
  }
  func application(_ application: UIApplication, configurationForConnecting session: UISceneSession, options: UIScene.ConnectionOptions) -> UISceneConfiguration {
    let configuration = UISceneConfiguration(name: "ExportFixture", sessionRole: session.role)
    configuration.delegateClass = ExportFixtureScene.self
    return configuration
  }
}

final class ExportFixtureScene: UIResponder, UIWindowSceneDelegate {
  var window: UIWindow?
  func scene(_ scene: UIScene, willConnectTo session: UISceneSession, options: UIScene.ConnectionOptions) {
    guard let scene = scene as? UIWindowScene else { return }
    let controller = UIViewController()
    controller.view.backgroundColor = .systemBackground
    let label = UILabel(frame: CGRect(x: 20, y: 100, width: 340, height: 160))
    label.text = "IM Export Fixture\nOnly synthetic export data\nExportTarget"
    label.numberOfLines = 0
    controller.view.addSubview(label)
    window = UIWindow(windowScene: scene)
    window?.rootViewController = controller
    window?.makeKeyAndVisible()
  }
}

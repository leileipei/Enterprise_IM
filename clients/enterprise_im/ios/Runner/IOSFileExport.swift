import Flutter
import UIKit
import UniformTypeIdentifiers

// The picker grants access to a folder before Dart opens its authorized source.
// Only opaque tickets and bounded bytes cross the channel, never private paths.
final class IOSFileExport: NSObject, UIDocumentPickerDelegate {
  private var channel: FlutterMethodChannel!
  private var choosing: FlutterResult?
  private var name: String?
  private var picker: UIDocumentPickerViewController?
  private var session: NativeExportSession?
  private var ticket: String?
  private var folder: URL?
  private var scoped = false
  private var background: NSObjectProtocol?

  init(messenger: FlutterBinaryMessenger) {
    super.init()
    channel = FlutterMethodChannel(name: "enterprise_im/file_export", binaryMessenger: messenger)
    channel.setMethodCallHandler { [weak self] call, result in
      guard let self = self else { result(FlutterError(code: "export_unavailable", message: nil, details: nil)); return }
      self.handle(call, result: result)
    }
    background = NotificationCenter.default.addObserver(
      forName: UIApplication.didEnterBackgroundNotification, object: nil, queue: .main
    ) { [weak self] _ in self?.cancelAll() }
  }

  private func fail(_ result: FlutterResult, _ error: Error? = nil) {
    let exists = (error as? NativeExportSession.Failure) == .exists
    result(FlutterError(code: exists ? "export_exists" : "export_unavailable",
      message: exists ? "同名文件已存在，请更改文件名或选择其他文件夹。" : "无法导出，请选择可写入的文件夹后重试。", details: nil))
  }

  private func handle(_ call: FlutterMethodCall, result: @escaping FlutterResult) {
    let arguments = call.arguments as? [String: Any]
    if call.method == "beginExport" {
      guard choosing == nil, session == nil else { fail(result); return }
      guard let name = arguments?["name"] as? String, NativeExportSession.validName(name) else { fail(result); return }
      let scene = UIApplication.shared.connectedScenes.compactMap { $0 as? UIWindowScene }
        .first { $0.activationState == .foregroundActive }
      guard var presenter = scene?.windows.first(where: { $0.isKeyWindow })?.rootViewController else { fail(result); return }
      while let presented = presenter.presentedViewController { presenter = presented }
      let picker = UIDocumentPickerViewController(forOpeningContentTypes: [.folder], asCopy: false)
      picker.delegate = self
      picker.allowsMultipleSelection = false
      picker.shouldShowFileExtensions = true
      picker.title = "选择导出文件夹"
      self.name = name; self.choosing = result; self.picker = picker
      presenter.present(picker, animated: true)
      return
    }
    guard ["writeExport", "finishExport", "abortExport"].contains(call.method) else {
      result(FlutterMethodNotImplemented); return
    }
    guard let supplied = arguments?["ticket"] as? String, supplied == ticket, let session = session else {
      if call.method == "abortExport" { result(nil) } else { fail(result) }
      return
    }
    do {
      switch call.method {
      case "writeExport":
        guard let bytes = arguments?["bytes"] as? FlutterStandardTypedData else { throw NativeExportSession.Failure.invalidState }
        try session.write(bytes.data)
      case "finishExport":
        try coordinate { _ in try session.finish() }
        try release()
      default: try release()
      }
      result(nil)
    } catch { failAfterRelease(result, error) }
  }

  func documentPicker(_ controller: UIDocumentPickerViewController, didPickDocumentsAt urls: [URL]) {
    guard let result = choosing, let name = name else { return }
    choosing = nil; self.name = nil; picker = nil
    guard urls.count == 1, let url = urls.first, url.isFileURL else { fail(result); return }
    folder = url; scoped = url.startAccessingSecurityScopedResource()
    do {
      try coordinate { coordinated in self.session = try NativeExportSession(directory: coordinated, name: name) }
      ticket = UUID().uuidString
      result(ticket)
    } catch { failAfterRelease(result, error) }
  }

  func documentPickerWasCancelled(_ controller: UIDocumentPickerViewController) {
    let result = choosing
    choosing = nil; name = nil; picker = nil
    result?(nil)
  }

  private func coordinate(_ body: (URL) throws -> Void) throws {
    guard let folder = folder else { throw NativeExportSession.Failure.invalidState }
    var coordinatorError: NSError?
    var operationError: Error?
    NSFileCoordinator().coordinate(writingItemAt: folder, options: [], error: &coordinatorError) { url in
      do { try body(url) } catch { operationError = error }
    }
    if let error = operationError { throw error }
    if let error = coordinatorError { throw error }
  }

  private func failAfterRelease(_ result: FlutterResult, _ original: Error) {
    do { try release(); fail(result, original) }
    catch {
      result(FlutterError(code: "export_cleanup_failed", message: "临时文件清理未完成，请恢复目标文件夹写入权限后重试。", details: nil))
    }
  }

  private func release() throws {
    try session?.abort(); session = nil; ticket = nil
    if scoped { folder?.stopAccessingSecurityScopedResource() }
    folder = nil; scoped = false
  }

  private func cancelAll() {
    let result = choosing
    choosing = nil; name = nil
    picker?.dismiss(animated: false); picker = nil
    try? release(); result?(nil)
  }

  deinit {
    if let background = background { NotificationCenter.default.removeObserver(background) }
    cancelAll()
    if scoped { folder?.stopAccessingSecurityScopedResource() }
    channel.setMethodCallHandler(nil)
  }
}

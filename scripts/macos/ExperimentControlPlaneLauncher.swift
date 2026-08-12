import AppKit
import Darwin
import Foundation
import Security
import WebKit

private enum LauncherError: LocalizedError {
    case missingResource(String)
    case applicationSupport(Error)
    case lock(Error)
    case alreadyRunning
    case processStart(String, Error)
    case controlPlaneExited(Int32)
    case healthTimeout(String)
    case invalidListenAddress(String)
    case randomToken(OSStatus)

    var errorDescription: String? {
        switch self {
        case .missingResource(let name):
            return "The app bundle is incomplete: \(name) is missing."
        case .applicationSupport(let error):
            return "The application data folder could not be prepared: \(error.localizedDescription)"
        case .lock(let error):
            return "The single-instance lock could not be opened: \(error.localizedDescription)"
        case .alreadyRunning:
            return "Experiment Control Plane is already running for this user."
        case .processStart(let name, let error):
            return "\(name) could not be started: \(error.localizedDescription)"
        case .controlPlaneExited(let status):
            return "The control plane exited before it became ready (status \(status))."
        case .healthTimeout(let logPath):
            return "The control plane did not become ready in time. See \(logPath)."
        case .invalidListenAddress(let value):
            return "The control plane published an invalid loopback address: \(value)."
        case .randomToken(let status):
            return "A secure local API token could not be generated (status \(status))."
        }
    }
}

private final class InstanceLock {
    private var descriptor: Int32 = -1

    init(url: URL) throws {
        descriptor = Darwin.open(url.path, O_CREAT | O_RDWR, S_IRUSR | S_IWUSR)
        guard descriptor >= 0 else {
            throw LauncherError.lock(NSError(domain: NSPOSIXErrorDomain, code: Int(errno)))
        }
        guard flock(descriptor, LOCK_EX | LOCK_NB) == 0 else {
            Darwin.close(descriptor)
            descriptor = -1
            if errno == EWOULDBLOCK {
                throw LauncherError.alreadyRunning
            }
            throw LauncherError.lock(NSError(domain: NSPOSIXErrorDomain, code: Int(errno)))
        }
    }

    deinit {
        if descriptor >= 0 {
            _ = flock(descriptor, LOCK_UN)
            Darwin.close(descriptor)
        }
    }
}

@MainActor
private final class ProcessSupervisor {
    private enum ShutdownState {
        case running
        case stopping
        case stopped
    }

    private var processes: [Process] = []
    private var shutdownState = ShutdownState.running
    private var stopCompletions: [@MainActor @Sendable () -> Void] = []

    var controlPlane: Process?

    func launch(
        executable: URL,
        arguments: [String],
        environment: [String: String],
        standardOutput: FileHandle,
        standardError: FileHandle,
        terminationHandler: (@Sendable (Process) -> Void)? = nil
    ) throws -> Process {
        let process = Process()
        process.executableURL = executable
        process.arguments = arguments
        process.environment = environment
        process.standardOutput = standardOutput
        process.standardError = standardError
        process.terminationHandler = terminationHandler
        do {
            try process.run()
        } catch {
            throw LauncherError.processStart(executable.lastPathComponent, error)
        }
        processes.append(process)
        return process
    }

    func stopAll(completion: (@MainActor @Sendable () -> Void)? = nil) {
        if let completion { stopCompletions.append(completion) }
        switch shutdownState {
        case .stopping:
            return
        case .stopped:
            DispatchQueue.main.async { [weak self] in self?.drainStopCompletions() }
            return
        case .running:
            shutdownState = .stopping
        }

        let running = processes.reversed().filter(\.isRunning)
        for process in running {
            process.terminate()
        }
        guard !running.isEmpty else {
            DispatchQueue.main.async { [weak self] in self?.finishStopping() }
            return
        }
        DispatchQueue.global(qos: .utility).async {
            let deadline = Date().addingTimeInterval(3)
            while Date() < deadline && running.contains(where: \.isRunning) { usleep(50_000) }
            for process in running where process.isRunning { kill(process.processIdentifier, SIGKILL) }
            DispatchQueue.main.async { [weak self] in self?.finishStopping() }
        }
    }

    private func finishStopping() {
        shutdownState = .stopped
        drainStopCompletions()
    }

    private func drainStopCompletions() {
        let completions = stopCompletions
        stopCompletions.removeAll()
        for completion in completions { completion() }
    }
}

private final class LauncherViewController: NSViewController, WKNavigationDelegate {
    private let webView: WKWebView
    private let progress = NSProgressIndicator()
    private let message = NSTextField(labelWithString: "Starting your local control plane…")
    private let statusContainer = NSVisualEffectView()
    private var allowedOrigin: (scheme: String, host: String, port: Int)?

    init(apiToken: String) {
        let configuration = WKWebViewConfiguration()
        configuration.websiteDataStore = .nonPersistent()
        let encodedToken = Self.javaScriptStringLiteral(apiToken)
        configuration.userContentController.addUserScript(WKUserScript(
            source: "sessionStorage.setItem('controlPlaneApiToken', \(encodedToken));",
            injectionTime: .atDocumentStart,
            forMainFrameOnly: true
        ))
        webView = WKWebView(frame: .zero, configuration: configuration)
        super.init(nibName: nil, bundle: nil)
        webView.navigationDelegate = self
    }

    @available(*, unavailable)
    required init?(coder: NSCoder) { fatalError("init(coder:) is unavailable") }

    private static func javaScriptStringLiteral(_ value: String) -> String {
        let data = try! JSONSerialization.data(withJSONObject: [value])
        let array = String(decoding: data, as: UTF8.self)
        return String(array.dropFirst().dropLast())
    }

    override func loadView() {
        let root = NSView()
        root.wantsLayer = true
        webView.translatesAutoresizingMaskIntoConstraints = false
        webView.isHidden = true
        root.addSubview(webView)

        statusContainer.translatesAutoresizingMaskIntoConstraints = false
        statusContainer.material = .underWindowBackground
        statusContainer.blendingMode = .withinWindow
        statusContainer.state = .active
        root.addSubview(statusContainer)

        progress.style = .spinning
        progress.controlSize = .regular
        progress.translatesAutoresizingMaskIntoConstraints = false
        progress.startAnimation(nil)
        message.font = .systemFont(ofSize: 15, weight: .medium)
        message.textColor = .secondaryLabelColor
        message.alignment = .center
        message.maximumNumberOfLines = 2
        message.translatesAutoresizingMaskIntoConstraints = false
        statusContainer.addSubview(progress)
        statusContainer.addSubview(message)

        NSLayoutConstraint.activate([
            webView.leadingAnchor.constraint(equalTo: root.leadingAnchor),
            webView.trailingAnchor.constraint(equalTo: root.trailingAnchor),
            webView.topAnchor.constraint(equalTo: root.topAnchor),
            webView.bottomAnchor.constraint(equalTo: root.bottomAnchor),
            statusContainer.leadingAnchor.constraint(equalTo: root.leadingAnchor),
            statusContainer.trailingAnchor.constraint(equalTo: root.trailingAnchor),
            statusContainer.topAnchor.constraint(equalTo: root.topAnchor),
            statusContainer.bottomAnchor.constraint(equalTo: root.bottomAnchor),
            progress.centerXAnchor.constraint(equalTo: statusContainer.centerXAnchor),
            progress.centerYAnchor.constraint(equalTo: statusContainer.centerYAnchor, constant: -16),
            message.centerXAnchor.constraint(equalTo: statusContainer.centerXAnchor),
            message.topAnchor.constraint(equalTo: progress.bottomAnchor, constant: 16),
            message.leadingAnchor.constraint(greaterThanOrEqualTo: statusContainer.leadingAnchor, constant: 32),
            message.trailingAnchor.constraint(lessThanOrEqualTo: statusContainer.trailingAnchor, constant: -32),
        ])
        view = root
    }

    func load(_ url: URL) {
        allowedOrigin = (url.scheme ?? "", url.host ?? "", url.port ?? -1)
        webView.load(URLRequest(url: url, cachePolicy: .reloadIgnoringLocalCacheData))
    }

    func showError(_ text: String) {
        progress.stopAnimation(nil)
        progress.isHidden = true
        message.stringValue = text
        message.textColor = .systemRed
        statusContainer.isHidden = false
        webView.isHidden = true
    }

    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
        statusContainer.isHidden = true
        webView.isHidden = false
        view.window?.makeFirstResponder(webView)
    }

    func webView(
        _ webView: WKWebView,
        decidePolicyFor navigationAction: WKNavigationAction,
        decisionHandler: @escaping @MainActor (WKNavigationActionPolicy) -> Void
    ) {
        guard let url = navigationAction.request.url else {
            decisionHandler(.cancel)
            return
        }
        if let origin = allowedOrigin,
           url.scheme == origin.scheme, url.host == origin.host, (url.port ?? -1) == origin.port {
            decisionHandler(.allow)
            return
        }
        if url.scheme == "http" || url.scheme == "https" {
            NSWorkspace.shared.open(url)
            decisionHandler(.cancel)
            return
        }
        decisionHandler(.cancel)
    }
}

@main @MainActor
private final class AppDelegate: NSObject, NSApplicationDelegate, NSWindowDelegate {
    private var window: NSWindow!
    private var viewController: LauncherViewController!
    private let supervisor = ProcessSupervisor()
    private var instanceLock: InstanceLock?
    private var logHandle: FileHandle?
    private var applicationSupportURL: URL?
    private var localURL: URL?
    private var startupTask: Task<Void, Never>?
    private var terminating = false
    private var terminationReplyPending = false
    private var apiToken = ""

    static func main() {
        let delegate = AppDelegate()
        NSApplication.shared.delegate = delegate
        NSApplication.shared.setActivationPolicy(.regular)
        NSApplication.shared.run()
        withExtendedLifetime(delegate) {}
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        do {
            apiToken = try Self.generateAPIToken()
        } catch {
            presentErrorAlert(title: "Experiment Control Plane Couldn’t Start", message: error.localizedDescription)
            NSApp.terminate(nil)
            return
        }
        viewController = LauncherViewController(apiToken: apiToken)
        configureApplicationMenu()
        createWindow()
        startupTask = Task { [weak self] in
            await self?.startServices()
        }
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { true }

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        if terminationReplyPending { return .terminateLater }
        terminating = true
        startupTask?.cancel()
        terminationReplyPending = true
        supervisor.stopAll {
            NSApp.reply(toApplicationShouldTerminate: true)
        }
        return .terminateLater
    }

    func applicationWillTerminate(_ notification: Notification) {
        terminating = true
        startupTask?.cancel()
        try? logHandle?.close()
    }

    func windowShouldClose(_ sender: NSWindow) -> Bool {
        NSApp.terminate(nil)
        return false
    }

    private func createWindow() {
        window = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 1180, height: 760),
            styleMask: [.titled, .closable, .miniaturizable, .resizable, .fullSizeContentView],
            backing: .buffered,
            defer: false
        )
        window.title = "Experiment Control Plane"
        window.titlebarAppearsTransparent = true
        window.titleVisibility = .visible
        window.minSize = NSSize(width: 880, height: 600)
        window.contentViewController = viewController
        window.delegate = self
        window.center()
        window.setFrameAutosaveName("ExperimentControlPlane.MainWindow")
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    private func configureApplicationMenu() {
        let mainMenu = NSMenu()
        let appMenuItem = NSMenuItem()
        mainMenu.addItem(appMenuItem)
        let appMenu = NSMenu()
        appMenu.addItem(withTitle: "About Experiment Control Plane", action: #selector(NSApplication.orderFrontStandardAboutPanel(_:)), keyEquivalent: "")
        appMenu.addItem(.separator())
        appMenu.addItem(withTitle: "Quit Experiment Control Plane", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        appMenuItem.submenu = appMenu
        NSApp.mainMenu = mainMenu
    }

    @MainActor
    private func startServices() async {
        do {
            let setup = try prepareRuntime()
            try launchControlPlane(setup: setup)
            let resolvedURL = try await waitForListenAddress(file: setup.listenAddressFile, timeout: 10)
            localURL = resolvedURL
            try await waitUntilHealthy(url: resolvedURL.appendingPathComponent("healthz"), timeout: 10)
            try Task.checkCancellation()
            try launchDemoWorker(setup: setup, localURL: resolvedURL)
            viewController.load(resolvedURL)
        } catch LauncherError.alreadyRunning {
            if let bundleIdentifier = Bundle.main.bundleIdentifier, !bundleIdentifier.isEmpty {
                NSRunningApplication.runningApplications(withBundleIdentifier: bundleIdentifier)
                    .first(where: { $0.processIdentifier != ProcessInfo.processInfo.processIdentifier })?
                    .activate(options: [.activateAllWindows])
            }
            // No child processes or shared resources were acquired. Exiting
            // directly avoids re-entering the deferred termination handshake
            // while another launcher owns the single-instance lock.
            Darwin.exit(EXIT_SUCCESS)
        } catch is CancellationError where terminating {
            return
        } catch {
            showStartupError(error)
        }
    }

    private struct RuntimeSetup {
        let controlPlane: URL
        let worker: URL
        let data: URL
        let artifacts: URL
        let listenAddressFile: URL
        let environment: [String: String]
    }

    private func prepareRuntime() throws -> RuntimeSetup {
        guard let controlPlane = auxiliaryExecutable(named: "controlplane") else {
            throw LauncherError.missingResource("controlplane")
        }
        guard let worker = auxiliaryExecutable(named: "worker") else {
            throw LauncherError.missingResource("worker")
        }
        let base: URL
        do {
            base = try FileManager.default.url(
                for: .applicationSupportDirectory,
                in: .userDomainMask,
                appropriateFor: nil,
                create: true
            ).appendingPathComponent("Experiment Control Plane", isDirectory: true)
            let privateDirectories = [
                base,
                base.appendingPathComponent("artifacts", isDirectory: true),
                base.appendingPathComponent("logs", isDirectory: true),
            ]
            for directory in privateDirectories {
                try FileManager.default.createDirectory(
                    at: directory,
                    withIntermediateDirectories: true,
                    attributes: [.posixPermissions: 0o700]
                )
                try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: directory.path)
            }
            let runtime = base.appendingPathComponent("runtime", isDirectory: true)
            try FileManager.default.createDirectory(
                at: runtime,
                withIntermediateDirectories: true,
                attributes: [.posixPermissions: 0o700]
            )
            try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: runtime.path)
        } catch {
            throw LauncherError.applicationSupport(error)
        }
        applicationSupportURL = base
        instanceLock = try InstanceLock(url: base.appendingPathComponent("launcher.lock"))
        let inherited = ProcessInfo.processInfo.environment
        var environment: [String: String] = [:]
        for key in ["PATH", "HOME", "TMPDIR"] {
            if let value = inherited[key], !value.isEmpty { environment[key] = value }
        }
        environment["LC_ALL"] = "en_US.UTF-8"
        environment["LANG"] = "en_US.UTF-8"
        environment["CONTROL_PLANE_API_TOKEN"] = apiToken
        return RuntimeSetup(
            controlPlane: controlPlane,
            worker: worker,
            data: base.appendingPathComponent("control-plane.json"),
            artifacts: base.appendingPathComponent("artifacts", isDirectory: true),
            listenAddressFile: base.appendingPathComponent("runtime/listen-address"),
            environment: environment
        )
    }

    private func auxiliaryExecutable(named name: String) -> URL? {
        if let url = Bundle.main.url(forAuxiliaryExecutable: name) { return url }
        let fallback = Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/\(name)")
        return FileManager.default.isExecutableFile(atPath: fallback.path) ? fallback : nil
    }

    private func openLogHandle() throws -> FileHandle {
        if let logHandle { return logHandle }
        guard let base = applicationSupportURL else { throw LauncherError.missingResource("application support directory") }
        let url = base.appendingPathComponent("logs/launcher.log")
        if !FileManager.default.fileExists(atPath: url.path) {
            FileManager.default.createFile(atPath: url.path, contents: nil, attributes: [.posixPermissions: 0o600])
        }
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
        let handle = try FileHandle(forWritingTo: url)
        try handle.seekToEnd()
        logHandle = handle
        return handle
    }

    private func launchControlPlane(setup: RuntimeSetup) throws {
        do {
            try FileManager.default.removeItem(at: setup.listenAddressFile)
        } catch CocoaError.fileNoSuchFile {
            // No stale handshake file is the normal first-launch state.
        }
        let handle = try openLogHandle()
        let process = try supervisor.launch(
            executable: setup.controlPlane,
            arguments: [
                "-listen", "127.0.0.1:0",
                "-listen-address-file", setup.listenAddressFile.path,
                "-data", setup.data.path,
            ],
            environment: setup.environment,
            standardOutput: handle,
            standardError: handle,
            terminationHandler: { [weak self] process in
                DispatchQueue.main.async {
                    guard let self, !self.terminating else { return }
                    self.supervisor.stopAll()
                    self.viewController.showError("The local control plane stopped unexpectedly (status \(process.terminationStatus)).")
                    self.presentErrorAlert(
                        title: "Control Plane Stopped",
                        message: "The bundled control plane exited unexpectedly. You can quit and reopen the app."
                    )
                }
            }
        )
        supervisor.controlPlane = process
    }

    private func launchDemoWorker(setup: RuntimeSetup, localURL: URL) throws {
        let handle = try openLogHandle()
        _ = try supervisor.launch(
            executable: setup.worker,
            arguments: [
                "-control-plane", localURL.absoluteString,
                "-id", "macos-app-demo-worker",
                "-name", "This Mac (Demo)",
                "-adapter", "demo.sleep",
                "-labels", "os=darwin,accelerator=cpu,launcher=macos-app",
                "-artifacts", setup.artifacts.path,
            ],
            environment: setup.environment,
            standardOutput: handle,
            standardError: handle,
            terminationHandler: { [weak self] process in
                DispatchQueue.main.async {
                    guard let self, !self.terminating else { return }
                    self.viewController.showError("The local demo worker stopped unexpectedly (status \(process.terminationStatus)).")
                    self.presentErrorAlert(
                        title: "Demo Worker Stopped",
                        message: "The control plane is still running, but local demo jobs cannot start until you reopen the app."
                    )
                }
            }
        )
    }

    private func waitUntilHealthy(url: URL, timeout: TimeInterval) async throws {
        let session = URLSession(configuration: .ephemeral)
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            try Task.checkCancellation()
            if let process = supervisor.controlPlane, !process.isRunning {
                throw LauncherError.controlPlaneExited(process.terminationStatus)
            }
            do {
                let (_, response) = try await session.data(from: url)
                if (response as? HTTPURLResponse)?.statusCode == 200 { return }
            } catch {
                // Startup connection failures are expected until the listener binds.
            }
            try await Task.sleep(for: .milliseconds(150))
        }
        let logPath = applicationSupportURL?.appendingPathComponent("logs/launcher.log").path ?? "the launcher log"
        throw LauncherError.healthTimeout(logPath)
    }

    private func showStartupError(_ error: Error) {
        supervisor.stopAll()
        let message = (error as? LocalizedError)?.errorDescription ?? error.localizedDescription
        viewController.showError(message)
        presentErrorAlert(title: "Experiment Control Plane Couldn’t Start", message: message)
    }

    private func presentErrorAlert(title: String, message: String) {
        let alert = NSAlert()
        alert.alertStyle = .critical
        alert.messageText = title
        alert.informativeText = message
        alert.addButton(withTitle: "OK")
        if let applicationSupportURL {
            alert.addButton(withTitle: "Show Logs")
            let response = alert.runModal()
            if response == .alertSecondButtonReturn {
                NSWorkspace.shared.activateFileViewerSelecting([applicationSupportURL.appendingPathComponent("logs/launcher.log")])
            }
        } else {
            alert.runModal()
        }
    }

    private func waitForListenAddress(file: URL, timeout: TimeInterval) async throws -> URL {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            try Task.checkCancellation()
            if let process = supervisor.controlPlane, !process.isRunning {
                throw LauncherError.controlPlaneExited(process.terminationStatus)
            }
            if let text = try? String(contentsOf: file, encoding: .utf8).trimmingCharacters(in: .whitespacesAndNewlines),
               !text.isEmpty {
                guard let separator = text.lastIndex(of: ":"),
                      String(text[..<separator]) == "127.0.0.1",
                      let port = Int(text[text.index(after: separator)...]), (1...65535).contains(port),
                      let url = URL(string: "http://127.0.0.1:\(port)/") else {
                    throw LauncherError.invalidListenAddress(text)
                }
                return url
            }
            try await Task.sleep(for: .milliseconds(50))
        }
        let logPath = applicationSupportURL?.appendingPathComponent("logs/launcher.log").path ?? "the launcher log"
        throw LauncherError.healthTimeout(logPath)
    }

    private static func generateAPIToken() throws -> String {
        var bytes = [UInt8](repeating: 0, count: 32)
        let status = SecRandomCopyBytes(kSecRandomDefault, bytes.count, &bytes)
        guard status == errSecSuccess else { throw LauncherError.randomToken(status) }
        return bytes.map { String(format: "%02x", $0) }.joined()
    }
}
